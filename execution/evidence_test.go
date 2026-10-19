package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestProviderEvidenceConcurrentAttemptsAndUnknownDefault(t *testing.T) {
	store := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
	// This test checks isolation and terminal records, not disk throughput.
	// Start writers together and let completion drive the assertions; the test
	// context bounds a stuck write without a per-write wall-clock race.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a, err := Reserve(t.Context(), Metadata{Engine: "offline"})
			if err != nil {
				t.Error(err)
				return
			}
			a.evidenceContext = t.Context()
			if err := a.UpdateEvidence(func(e *ProviderEvidence) {
				e.Model = "observed-model"
				tokens := int64(34)
				e.Tokens = &tokens
			}); err != nil {
				t.Error(err)
				return
			}
			path := filepath.Join(store, a.ID+".json")
			before, err := ReadEvidence(path)
			if err != nil {
				t.Error(err)
				return
			}
			if before.Status != Unknown || before.OutcomeKnown {
				t.Error("initial record claimed completion")
			}
			if err := a.Complete(Outcome{Status: Timeout, Known: false}); err != nil {
				t.Error(err)
				return
			}
			after, err := ReadEvidence(path)
			if err != nil {
				t.Error(err)
				return
			}
			if after.Status != Timeout || after.OutcomeKnown || after.Model != "observed-model" || after.Tokens == nil || *after.Tokens != 34 {
				t.Errorf("terminal record: %+v", after)
			}
		}()
	}
	close(start)
	wg.Wait()
	paths, _ := filepath.Glob(filepath.Join(store, "*.json"))
	if len(paths) != 8 {
		t.Fatalf("records clobbered: %d", len(paths))
	}
}

func TestEvidenceWriteUsesIndependentAccountingDeadline(t *testing.T) {
	before := time.Now()
	ctx, cancel := (&Attempt{}).evidenceWriteContext()
	defer cancel()
	after := time.Now()
	deadline, ok := ctx.Deadline()
	// Bracket creation instead of requiring the assertion to run within a
	// particular interval after it (the race scheduler may pause this test).
	if !ok || deadline.Before(before.Add(accountingWriteTimeout)) || deadline.After(after.Add(accountingWriteTimeout)) {
		t.Fatalf("evidence deadline %v, want accounting budget %v from creation", deadline, accountingWriteTimeout)
	}
}

func TestEvidenceDeadlineKeepsStartedCallCharge(t *testing.T) {
	path := layeredFixture(t)
	store := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "deadline-evidence", MaxCalls: 100})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateEvidence(func(*ProviderEvidence) {}); err != nil {
		t.Fatal(err)
	}
	// Inject an already expired deadline: no sleep, lock race, or slow disk is
	// needed to exercise the exact production timeout error and settlement path.
	evidenceCtx, cancel := context.WithDeadline(t.Context(), time.Time{})
	defer cancel()
	a.evidenceContext = evidenceCtx
	if err := a.UpdateEvidence(func(e *ProviderEvidence) { e.Model = "unpersisted" }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("evidence deadline lost: %v", err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("completion hid evidence deadline: %v", err)
	}
	select {
	case <-a.leaseStop:
	default:
		t.Error("heartbeat still active after evidence deadline")
	}
	record, err := ReadEvidence(filepath.Join(store, a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != Unknown || record.OutcomeKnown || record.Model != "" {
		t.Fatalf("expired write published evidence: %+v", record)
	}
	data := readTestLedger(t, path)
	entry := data["attempts"].([]any)[0].(map[string]any)
	if entry["result_status"] != "UNKNOWN" || entry["outcome_known"] != false || len(data["leases"].(map[string]any)) != 0 {
		t.Fatalf("bad settlement after evidence deadline: %v", data)
	}
	if got := data["scopes"].(map[string]any)["tasks"].(map[string]any)["deadline-evidence"].(map[string]any)["used"]; got != float64(1) {
		t.Fatalf("started call refunded after evidence deadline: %v", got)
	}
}

func TestProviderEvidenceFailureCannotPublishKnownCompletion(t *testing.T) {
	store := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
	a, err := Reserve(context.Background(), Metadata{Engine: "offline"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateEvidence(func(*ProviderEvidence) {}); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", filepath.Join(blocker, "child"))
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err == nil {
		t.Fatal("evidence failure accepted as success")
	}
	record, err := ReadEvidence(filepath.Join(store, a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if record.OutcomeKnown || record.Status != Unknown {
		t.Fatalf("completion falsely persisted: %+v", record)
	}
}

func TestEvidenceFailureSettlesLeaseAndStopsHeartbeat(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	store := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "evidence-cleanup", MaxCalls: 100})
	a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", filepath.Join(blocker, "child"))
	if err := a.UpdateEvidence(func(*ProviderEvidence) {}); err == nil {
		t.Fatal("expected evidence error")
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err == nil {
		t.Fatal("must report evidence error")
	}
	select {
	case <-a.leaseStop:
	default:
		t.Error("heartbeat still active")
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", store)
	next, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
	if err != nil {
		t.Fatalf("capacity leaked: %v", err)
	}
	_ = next.ReleaseUnstarted()
	if got := readTestLedger(t, path)["scopes"].(map[string]any)["tasks"].(map[string]any)["evidence-cleanup"].(map[string]any)["used"]; got != float64(0) {
		t.Fatalf("unstarted calls were not refunded: %v", got)
	}
}

func TestEvidenceFailureKeepsStartedCallCharge(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", t.TempDir())
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "started-evidence", MaxCalls: 100})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", filepath.Join(blocker, "child"))
	_ = a.UpdateEvidence(func(*ProviderEvidence) {})
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err == nil {
		t.Fatal("lost error")
	}
	_ = a.Complete(Outcome{Status: Passed, Known: true})
	data := readTestLedger(t, path)
	entry := data["attempts"].([]any)[0].(map[string]any)
	if entry["result_status"] != "UNKNOWN" || entry["outcome_known"] != false || len(data["leases"].(map[string]any)) != 0 {
		t.Fatalf("bad settlement: %v", data)
	}
	if got := data["scopes"].(map[string]any)["tasks"].(map[string]any)["started-evidence"].(map[string]any)["used"]; got != float64(1) {
		t.Fatalf("started call refunded: %v", got)
	}
}
