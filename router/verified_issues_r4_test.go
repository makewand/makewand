package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
)

func TestR4RouterDefaultBusyFallsBackImmediately(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[stream], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CODEX_HOME", "")
			t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
			t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
			t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
			t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "")
			t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
			ctx, path := budgetTestContext(t, 5)
			holder, err := execution.Reserve(ctx, execution.Metadata{Engine: "claude"})
			if err != nil {
				t.Fatal(err)
			}
			var finishOnce sync.Once
			finishHolder := func() {
				finishOnce.Do(func() {
					if err := holder.Complete(execution.Outcome{Status: execution.Passed, Known: true}); err != nil {
						t.Errorf("release holder: %v", err)
					}
				})
			}
			defer finishHolder()
			var primaryCalls, fallbackCalls atomic.Int32
			primary := &dispatchBudgetProvider{name: "claude", calls: &primaryCalls}
			fallback := &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls}
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
				"claude": {Provider: primary, Access: AccessSubscription},
				"gemini": {Provider: fallback, Access: AccessSubscription},
			}, DefaultModel: "claude", CodingModel: "claude"})
			// Enter the routing pipeline with a resolver we can observe. The
			// first resolution must happen inside the capacity probe, before
			// the primary's busy error. Release capacity at that event: ignoring
			// a true probe result would retry and dispatch the primary instead.
			probed, busyAfterProbe := false, false
			resolve := func(name, modelID string) (Provider, string, error) {
				probed = true
				finishHolder()
				return fallback, modelID, nil
			}
			r.SetTraceSink(TraceSinkFunc(func(event TraceEvent) {
				if event.Event == "r4_busy" && event.Selected == "claude" {
					busyAfterProbe = probed
				}
			}))
			ac := &attemptContext{ctx: ctx, task: TaskCode, labels: attemptLabels{attemptError: "r4_busy"}}
			selected := RouteResult{Actual: "claude", Provider: primary}
			candidates := []fallbackCandidate{{name: "gemini"}}
			if stream {
				var chunks <-chan StreamChunk
				chunks, _, err = r.routeAndExecuteStream(ac, selected, candidates, resolve)
				if err == nil {
					for chunk := range chunks {
						if chunk.Error != nil {
							err = chunk.Error
						}
					}
				}
			} else {
				_, _, _, err = r.routeAndExecute(ac, selected, candidates, resolve)
			}
			if err != nil || primaryCalls.Load() != 0 || fallbackCalls.Load() != 1 {
				t.Fatalf("busy fallback: %v, calls=%d/%d", err, primaryCalls.Load(), fallbackCalls.Load())
			}
			if !busyAfterProbe {
				t.Fatal("busy refusal did not consult capacity fallback before routing onward")
			}
			if len(budgetTestAttempts(t, path)) != 2 {
				t.Fatal("busy admission spent a call")
			}
		})
	}
}

// The deadline expires on the busy event, without a real timer or fsync
// headroom requirement. Admission must leave the independent caller usable.
type r4EventDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c r4EventDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c r4EventDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestR4BusyAttemptDeadlineDoesNotBecomeAccountingFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
	t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	ctx, path := budgetTestContext(t, 5)
	caller, cancel := context.WithCancel(ctx)
	defer cancel()
	holder, err := execution.Reserve(caller, execution.Metadata{Engine: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Complete(execution.Outcome{Status: execution.Passed, Known: true})
	liveAttempt, expire := context.WithCancel(caller)
	defer expire()
	var expiredOnBusy bool
	attemptCtx := execution.ContextWithCapacityFallback(r4EventDeadlineContext{
		Context: liveAttempt, deadline: time.Now().Add(time.Hour),
	}, func() bool {
		expiredOnBusy = true
		expire()
		return false
	})
	var calls atomic.Int32
	primary := &dispatchBudgetProvider{name: "claude", calls: &calls}
	_, _, err = reserveProvider(attemptCtx, primary)
	if !errors.Is(err, execution.ErrConcurrencyBusy) || errors.Is(err, errExecutionAccounting) || stopExecutionReplay(err) {
		t.Fatalf("busy admission became terminal: %v", err)
	}
	if !expiredOnBusy || caller.Err() != nil || !errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("deadline event was not isolated to admission: busy=%v caller=%v attempt=%v", expiredOnBusy, caller.Err(), attemptCtx.Err())
	}
	fallback := &dispatchBudgetProvider{name: "gemini", calls: &calls}
	if _, _, err := ChatProvider(caller, fallback, nil, "", 16); err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if calls.Load() != 1 || len(budgetTestAttempts(t, path)) != 2 {
		t.Fatal("busy admission dispatched or spent a call")
	}
}
