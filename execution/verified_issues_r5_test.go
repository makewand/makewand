package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestR5StartLockTimeoutStopsHeartbeatAndRefundsReservation(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 1, TaskID: "start-lock"})
	a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.leaseStopOnce.Do(func() { close(a.leaseStop) }) })
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- withLockedFile(t.Context(), path, nil, func(*accountingPath) error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("cannot hold ledger lock: %v", err)
	}
	// Start's independent lock budget is five seconds. Release can only
	// succeed after that first transition has failed; no timing race needed.
	timer := time.AfterFunc(5500*time.Millisecond, func() { close(release) })
	defer timer.Stop()
	err = a.Start()
	if lockErr := <-done; lockErr != nil {
		t.Fatal(lockErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start lock timeout was not exercised: %v", err)
	}
	select {
	case <-a.leaseStop:
	default:
		t.Fatal("failed Start kept renewing account")
	}
	data := readTestLedger(t, path)
	if data["attempts"].([]any)[0].(map[string]any)["status"] != "released" || len(data["leases"].(map[string]any)) != 0 {
		t.Fatalf("failed Start retained reservation / lease: %v", data)
	}
	next, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatalf("failed Start retained scope capacity: %v", err)
	}
	defer func() {
		if err := next.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
}

func TestR5ReleaseUnstartedKeepsRunningHeartbeat(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "running"})
	a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseUnstarted(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.leaseStop:
		t.Fatal("no-op release stopped a running provider's heartbeat")
	default:
	}
}

func TestR5StartFailureCannotRefundAlreadyCommittedReservation(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "already-started"})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	err = a.Start()
	stopped := false
	select {
	case <-a.leaseStop:
		stopped = true
	default:
	}
	//nolint:gosec // G703: restore the test-owned ledger returned by layeredFixture(t).
	if restoreErr := os.WriteFile(path, raw, 0600); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil || stopped {
		t.Fatalf("failed no-op Start refunded a previously committed dispatch: %v, stopped=%v", err, stopped)
	}
}

func TestR5FailedSettlementStopsHeartbeat(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "release"}[release], func(t *testing.T) {
			path := layeredFixture(t)
			ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "settle"})
			a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: release})
			if err != nil {
				t.Fatal(err)
			}
			// Also stop the old implementation's leaked goroutine on a red run.
			t.Cleanup(func() { a.leaseStopOnce.Do(func() { close(a.leaseStop) }) })
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if release {
				err = a.ReleaseUnstarted()
			} else {
				err = a.Complete(Outcome{Status: Passed, Known: true})
			}
			stopped := false
			select {
			case <-a.leaseStop:
				stopped = true
			default:
			}
			//nolint:gosec // G703: restore the test-owned ledger returned by layeredFixture(t).
			if restoreErr := os.WriteFile(path, raw, 0600); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if err == nil || !stopped {
				t.Fatalf("failed settlement kept its heartbeat: error=%v stopped=%v", err, stopped)
			}
			if !release {
				if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
					t.Fatal(err)
				}
				return
			}
			// The owner remains alive: stopping heartbeat alone cannot reclaim
			// this lease. Release must retry after the ledger becomes usable.
			deadline := time.Now().Add(3 * time.Second)
			for {
				data := readTestLedger(t, path)
				if data["attempts"].([]any)[0].(map[string]any)["status"] == "released" {
					if len(data["leases"].(map[string]any)) != 0 {
						t.Fatal("released attempt retained its account lease")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("transient release failure permanently reserved capacity")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// A one-call task can reserve again only if scopes were refunded.
			ctx = ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 1, TaskID: "settle"})
			next, err := Reserve(ctx, Metadata{Engine: "codex"})
			if err != nil {
				t.Fatalf("release did not refund scope capacity: %v", err)
			}
			defer func() {
				if err := next.Complete(Outcome{Status: Passed, Known: true}); err != nil {
					t.Fatal(err)
				}
			}()
		})
	}
}

func TestR5LeaseWaitDecimalContract(t *testing.T) {
	for _, tc := range []struct {
		value   string
		seconds float64
	}{
		{"", 30}, {" 5\t", 5}, {".5", .5}, {"+1e-2", .01}, {"-0", 0}, {"9223372036", 9223372036},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", tc.value)
			now := time.Now()
			until, err := leaseWaitDeadline(context.Background(), now)
			if err != nil || until.Sub(now) != time.Duration(tc.seconds*float64(time.Second)) {
				t.Fatalf("inconsistent lease wait: %v %v", until.Sub(now), err)
			}
		})
	}
	for _, value := range []string{" ", "invalid", "nan", "Inf", "-1", "1e309", "1e100", "9223372036.854776", "1_0", "５", "0x1p0", "\u00a05"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", value)
			_, err := leaseWaitDeadline(context.Background(), time.Now())
			if err == nil || ErrorStatus(err) != InternalError {
				t.Fatalf("invalid lease wait accepted or misclassified: %v", err)
			}
		})
	}
}

func TestR5InvalidLeaseWaitWithoutLedgerIsStillConfigurationError(t *testing.T) {
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "0")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "invalid")
	ctx := ContextWithConfig(t.Context(), Config{TaskID: "no-ledger"})
	_, err := Reserve(ctx, Metadata{Engine: "grok"})
	if err == nil || ErrorStatus(err) != InternalError {
		t.Fatalf("invalid lease wait ignored without a ledger: %v", err)
	}
}

func TestR5CompletionRetryOwnsOutcomeMeasurements(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "retry-outcome"})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	tokens := int64(42)
	if err := a.Complete(Outcome{Status: Passed, Known: true, Tokens: &tokens}); err == nil {
		t.Fatal("transient completion failure not exercised")
	}
	// Complete returned. The caller may reuse its usage storage while the
	// background cleanup must settle the original outcome.
	tokens = 999
	//nolint:gosec // G703: restore the test-owned ledger returned by layeredFixture(t).
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(3 * time.Second)
	for {
		entry := readTestLedger(t, path)["attempts"].([]any)[0].(map[string]any)
		if entry["status"] == "completed" {
			if entry["tokens"] != float64(42) {
				t.Fatalf("cleanup read caller's reused measurement: %v", entry["tokens"])
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("completion did not recover after ledger restoration")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestR5ReleaseRetrySurvivesTemporaryMissingDirectory(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "missing-directory"})
	a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	moved := dir + "-temporarily-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Stat(moved); err == nil {
			_ = os.Rename(moved, dir)
		}
	}()
	if err := a.ReleaseUnstarted(); err == nil {
		t.Fatal("missing ledger directory did not fail release")
	}
	// Outlast the initial 100ms retry. ENOENT can be transient too; the
	// same pinned directory must settle once it is restored, even later.
	time.Sleep(time.Second)
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(3 * time.Second)
	for readTestLedger(t, path)["attempts"].([]any)[0].(map[string]any)["status"] != "released" {
		if time.Now().After(until) {
			t.Fatal("cleanup abandoned a temporarily missing ledger directory")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
