package execution

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func layeredFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "10")
	t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "10")
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
	t.Setenv("MAKEWAND_BUDGET_TIMEZONE", "")
	return filepath.Join(t.TempDir(), "calls.json")
}
func TestLayeredIndependentTaskLimits(t *testing.T) {
	path := layeredFixture(t)
	small := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1, TaskID: "small"})
	if _, err := Reserve(small, Metadata{Engine: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(small, Metadata{Engine: "fake"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatal(err)
	}
	large := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 5, TaskID: "large"})
	for range 5 {
		if _, err := Reserve(large, Metadata{Engine: "fake"}); err != nil {
			t.Fatal(err)
		}
	}
	if readTestLedger(t, path)["maximum"] != float64(10) {
		t.Fatal("task maximum changed machine limit")
	}
}
func TestLayeredReleaseAndRepeatSettlement(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1, TaskID: "task"})
	a, err := Reserve(ctx, Metadata{Engine: "fake", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseUnstarted(); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseUnstarted(); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err == nil {
		t.Fatal("released reservation restarted")
	}
	a, err = Reserve(ctx, Metadata{Engine: "fake", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseUnstarted(); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Unknown, Known: false}); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "fake"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatal(err)
	}
}
func TestLayeredBusyAdmissionPersistsLowerLimitWithoutSpending(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 5, TaskID: "busy-limit"})
	first, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Complete(Outcome{Status: Passed, Known: true}) })
	lower := ContextWithConfig(ctx, Config{MaxCalls: 3})
	if _, err := Reserve(lower, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("busy admission: %v", err)
	}
	// A refusal must persist the tighter cap, without charging any scope.
	// Returning to the larger caller configuration cannot undo that cap.
	for _, engine := range []string{"grok", "muse"} {
		a, err := Reserve(ctx, Metadata{Engine: engine})
		if err != nil {
			t.Fatalf("busy admission spent capacity: %v", err)
		}
		t.Cleanup(func() { _ = a.Complete(Outcome{Status: Passed, Known: true}) })
	}
	if _, err := Reserve(ctx, Metadata{Engine: "agy"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("busy admission lost the lower limit: %v", err)
	}
}

// A caller refused as busy must not persist its lower lease cap for others.
func TestLayeredBusyRefusalDoesNotLowerSharedLeaseLimit(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "2")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "2")
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "busy-lease-limit"})
	first, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Complete(Outcome{Status: Passed, Known: true}) })
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("lower-limit caller should be busy: %v", err)
	}
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "2")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "2")
	second, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatalf("a busy refusal lowered the shared lease limit: %v", err)
	}
	t.Cleanup(func() { _ = second.Complete(Outcome{Status: Passed, Known: true}) })
}

func TestLayeredShanghaiDayAndClockRollback(t *testing.T) {
	layeredFixture(t)
	t.Setenv("MAKEWAND_BUDGET_TIMEZONE", "Asia/Shanghai")
	_, days := recordSkewReports(t)
	data := map[string]any{}
	if err := initializeScopes(data); err != nil {
		t.Fatal(err)
	}
	cfg := Config{TaskID: "clock", MaxCalls: 4}
	before := time.Date(2026, 10, 15, 15, 59, 59, 0, time.UTC)
	a, err := scopeKeys(data, cfg, before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scopeKeys(data, cfg, before.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a[2].([]any)[1] == b[2].([]any)[1] {
		t.Fatal("Shanghai midnight did not change daily scope")
	}
	// Back across that midnight: admitted, still counting into the later day.
	c, err := scopeKeys(data, cfg, before)
	if err != nil {
		t.Fatalf("backward day refused: %v", err)
	}
	if c[2].([]any)[1] != b[2].([]any)[1] || c[1].([]any)[1] != b[1].([]any)[1] || len(*days) != 1 {
		t.Fatalf("backward day reopened an earlier bucket: %v %v", c, *days)
	}
}

func TestMixedLayeredGoPythonProcessesShareHardLimit(t *testing.T) {
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "100")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "13")
	t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	TestMixedGoPythonProcessesShareHardLimit(t)
}
