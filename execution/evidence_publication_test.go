package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func failEvidenceDirectorySync(t *testing.T, dir string, failure func() error) {
	t.Helper()
	previous := accountingSyncDirectory
	accountingSyncDirectory = func(path *accountingPath) error {
		if path.root.Name() == dir && strings.HasSuffix(path.name, ".json") {
			return failure()
		}
		return previous(path)
	}
	t.Cleanup(func() { accountingSyncDirectory = previous })
}

func assertPendingEvidenceRetained(t *testing.T, dir, id string) {
	t.Helper()
	path := filepath.Join(dir, id+".json")
	if _, err := os.Stat(path + ".pending"); err != nil {
		t.Fatalf("publication uncertainty lost its marker: %v", err)
	}
	if _, err := ReadEvidence(path); err == nil {
		t.Fatal("Go trusted a record whose publication failed")
	}
	if out, err := pythonCommand(t, "from makewand import execution_evidence as e; assert not list(e.read_recent()); e.compact(force=True,now=__import__('time').time()+49*3600)").CombinedOutput(); err != nil {
		t.Fatalf("Python trusted/pruned pending evidence: %v %s", err, out)
	}
	if err := compactEvidence(dir, time.Now().Add(49*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retention deleted uncertain evidence: %v", err)
	}
}

func TestEvidenceFirstPublicationFailureIsUnknownAndCanRecover(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
	ledger := layeredFixture(t)
	a, err := Reserve(ContextWithConfig(t.Context(), Config{LedgerPath: ledger, TaskID: "publish", MaxCalls: 5}), Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	failing := true
	failEvidenceDirectorySync(t, dir, func() error {
		if failing {
			return errors.New("directory sync failed after record rename")
		}
		return nil
	})
	err = a.UpdateEvidence(func(*ProviderEvidence) {})
	var published *accountingPublishedError
	if !errors.As(err, &published) || errors.Is(err, ErrEvidenceDegraded) {
		t.Fatalf("published record incorrectly degraded: %v", err)
	}
	assertPendingEvidenceRetained(t, dir, a.ID)
	failing = false
	if err := a.complete(Outcome{Status: Passed, Known: true}); err == nil {
		t.Fatal("earlier publication failure was forgotten")
	}
	e := evidenceRecordAt(t, dir, a.ID)
	if e.Status != Unknown || e.OutcomeKnown || !e.Settled {
		t.Fatalf("recovered storage did not settle UNKNOWN: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(dir, a.ID+".json.pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful recovery retained pending marker: %v", err)
	}
	rows := readTestLedger(t, ledger)["attempts"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["result_status"] != "UNKNOWN" {
		t.Fatalf("publication failure changed accounting: %v", rows)
	}
}

func TestEvidencePublishedSettledSuccessCannotSurviveFailedCorrectionAsTrusted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
	ledger := layeredFixture(t)
	a := evidenceAttempt(t, ContextWithConfig(t.Context(), Config{LedgerPath: ledger, TaskID: "publish-final", MaxCalls: 5}), fixtureAccount)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failEvidenceDirectorySync(t, dir, func() error {
		raw, err := os.ReadFile(filepath.Join(dir, a.ID+".json"))
		if err != nil {
			return err
		}
		var value ProviderEvidence
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Settled && value.Status == Passed {
			// The PASSED rename happened. Prevent even the corrective UNKNOWN
			// write from publishing, as a continuing storage failure would.
			cancel()
			a.evidenceContext = ctx
			return errors.New("settled record directory sync failed")
		}
		return nil
	})
	if err := a.complete(Outcome{Status: Passed, Known: true}); err == nil {
		t.Fatal("final publication failure was swallowed")
	}
	rows := readTestLedger(t, ledger)["attempts"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["result_status"] != "UNKNOWN" {
		t.Fatalf("final failure did not downgrade the charged attempt: %v", rows)
	}
	assertPendingEvidenceRetained(t, dir, a.ID)
}

func TestEvidenceOrphanPendingCleanupHonorsRecordLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
	name := "orphan-publish.json"
	writeAged(t, filepath.Join(dir, name+".pending"), nil, time.Now().Add(-49*time.Hour))
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openExecutionFile(root, name+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if ok, err := tryLock(lock); err != nil || !ok {
		t.Fatalf("hold writer lock: %v %v", ok, err)
	}
	if out, err := pythonCommand(t, "from makewand import execution_evidence as e; e.compact(force=True)").CombinedOutput(); err != nil {
		t.Fatalf("Python compaction: %v %s", err, out)
	}
	if err := compactEvidence(dir, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat(name + ".pending"); err != nil {
		t.Fatalf("removed an active writer's pending marker: %v", err)
	}
	if err := unlock(lock); err != nil {
		t.Fatal(err)
	}
	if err := compactEvidence(dir, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat(name + ".pending"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan marker was not collected: %v", err)
	}
}
