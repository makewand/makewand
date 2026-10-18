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
func TestLayeredShanghaiDayAndClockRollback(t *testing.T) {
	layeredFixture(t)
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
	if _, err := scopeKeys(data, cfg, before); err == nil {
		t.Fatal("backward day admitted")
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
