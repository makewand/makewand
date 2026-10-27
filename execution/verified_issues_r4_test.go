package execution

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestR4DefaultBusyWaitLeavesDeadlineForFallback(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "")
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "holder", MaxCalls: 10})
	holder, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := holder.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	// The whole window stays inside the holder's 5 s lease heartbeat: a
	// heartbeat transaction landing on the last busy attempt (Windows scales
	// the window 25x) stalled it past the bound.
	window := min(capacityTestDuration(240*time.Millisecond), 4*time.Second)
	waitCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	started := time.Now()
	_, err = Reserve(waitCtx, Metadata{Engine: "codex"})
	if !errors.Is(err, ErrConcurrencyBusy) || errors.Is(err, ErrBudgetExhausted) || waitCtx.Err() != nil {
		t.Fatalf("busy wait consumed deadline or became terminal: %v, ctx=%v", err, waitCtx.Err())
	}
	if elapsed := time.Since(started); elapsed < window/8 || elapsed > window*11/12 {
		t.Fatalf("unexpected bounded wait: %v", elapsed)
	}
	fallback, err := Reserve(waitCtx, Metadata{Engine: "grok"})
	if err != nil {
		t.Fatalf("fallback admission failed: %v", err)
	}
	defer func() {
		if err := fallback.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	if len(readTestLedger(t, path)["attempts"].([]any)) != 2 {
		t.Fatal("busy admission was charged")
	}
}

// Looking for alternatives can probe CLI availability. Do it only on busy,
// never when the primary can dispatch or the real budget is exhausted.
func TestR4CapacityFallbackInspectedOnlyAfterBusy(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "10")
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "holder", MaxCalls: 10})
	checks := 0
	ctx = ContextWithCapacityFallback(ctx, func() bool {
		checks++
		return true
	})
	holder, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := holder.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	if checks != 0 {
		t.Fatal("successful admission probed fallbacks")
	}
	started := time.Now()
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("busy admission: %v", err)
	}
	// "Immediately" means without the 10 s lease wait; a fixed 100 ms bound
	// failed under -race on the Windows runner.
	if elapsed := time.Since(started); checks != 1 || elapsed > 5*time.Second {
		t.Fatalf("did not immediately use fallback: checks=%d after %s", checks, elapsed)
	}
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "1")
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("real exhaustion: %v", err)
	}
	if checks != 1 {
		t.Fatal("exhausted budget probed fallbacks")
	}
}
