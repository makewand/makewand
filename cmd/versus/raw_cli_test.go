package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/model"
)

// This test executable is the only child process used by the raw-arm tests.
// It avoids vendor CLIs, network access, and POSIX-only script fixtures.
func TestRawCLIChildStub(t *testing.T) {
	if os.Getenv("MAKEWAND_VERSUS_CHILD_STUB") != "1" {
		return
	}
	if os.Getenv("MAKEWAND_VERSUS_CHILD_MODE") == "initialization-delay" {
		// The true parent deadline must apply before any provider dispatch marker,
		// even when the Go runtime starts promptly on a fast host.
		time.Sleep(10 * time.Second)
	}
	path := os.Getenv("MAKEWAND_VERSUS_CHILD_STATE")
	// #nosec G703 -- child state is a parent-test-created temporary fixture path.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		os.Exit(3)
	}
	_, err = file.WriteString("dispatch\n")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		os.Exit(3)
	}
	switch os.Getenv("MAKEWAND_VERSUS_CHILD_MODE") {
	case "quota":
		fmt.Fprintln(os.Stderr, "synthetic quota exceeded")
		os.Exit(1)
	case "unknown":
		fmt.Fprintln(os.Stderr, "stream closed unexpectedly: Transport error (1007)")
		os.Exit(1)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "flood":
		block := strings.Repeat("x", 32<<10)
		for _, stream := range []*os.File{os.Stdout, os.Stderr} {
			for written := 0; written < 6<<20; written += len(block) {
				if _, err := stream.WriteString(block); err != nil {
					os.Exit(4)
				}
			}
		}
		// Only the capture supervisor may stop this fixture after overflow.
		// A transport/startup guard expiring must never look like that outcome.
		time.Sleep(30 * time.Second)
	case "marker":
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(path+".ready", []byte("ready"), 0o600); err != nil {
			os.Exit(4)
		}
		if !waitRawStubMarker(path, ".armed") {
			os.Exit(7)
		}
		// Start the escape window only after the parent has observed the real
		// descendant and explicitly released the leader-exit phase.
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(path+".armed-ready", []byte("armed"), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(2 * time.Second)
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(path+".escaped", []byte("escaped"), 0o600); err != nil {
			os.Exit(4)
		}
	case "leader":
		// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
		child := exec.Command(os.Args[0], rawStubArgs()...)
		child.Env = append(os.Environ(), "MAKEWAND_VERSUS_CHILD_MODE=marker")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if runtime.GOOS == "windows" {
			setProcessGroup(child)
		}
		if err := child.Start(); err != nil {
			os.Exit(5)
		}
		if !waitRawStubMarker(path, ".ready") {
			os.Exit(6)
		}
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(path+".leader-ready", []byte("ready"), 0o600); err != nil {
			os.Exit(4)
		}
		if !waitRawStubMarker(path, ".armed-ready") {
			os.Exit(7)
		}
		// #nosec G703 -- marker records the real leader's final exit path.
		if err := os.WriteFile(path+".leader-exit", []byte("exit"), 0o600); err != nil {
			os.Exit(4)
		}
		fmt.Print("leader\n")
		os.Exit(0)
	default:
		fmt.Println("--- FILE: fixture.txt ---\n```\nsynthetic fixture\n```")
	}
	os.Exit(0)
}

func waitRawStubMarker(path, suffix string) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// #nosec G703 -- markers are in the parent-created temporary fixture directory.
		if _, err := os.Stat(path + suffix); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestRawCLIOutputOverflowConsumesOneUnknownAttempt(t *testing.T) {
	bin, state, ledger, _ := setupRawStub(t, 3, "flood")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 30*time.Second)
	attempts := readRawAttempts(t, ledger)
	if ctx.Err() != nil || errors.Is(r.err, context.DeadlineExceeded) || errors.Is(r.err, context.Canceled) || !errors.Is(r.err, execution.ErrUnknownOutcome) || execution.ErrorStatus(r.err) != execution.Unknown || rawDispatchCount(t, state) != 1 || len(attempts) != 1 || attempts[0].Status != execution.Unknown || attempts[0].Known {
		t.Fatalf("overflow replayed, timed out, or marked complete: err=%v ctx=%v attempts=%+v", r.err, ctx.Err(), attempts)
	}
}

func TestRawCLILeaderExitClearsDescendantsAndPipes(t *testing.T) {
	bin, state, _, _ := setupRawStub(t, 3, "leader")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type completedRun struct {
		result result
		at     time.Time
	}
	done := make(chan completedRun, 1)
	go func() {
		r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 30*time.Second)
		done <- completedRun{result: r, at: time.Now()}
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("leader fixture did not finish after guard cancellation")
			}
		}
	}()
	for {
		if _, err := os.Stat(state + ".leader-ready"); err == nil {
			break
		}
		select {
		case completed := <-done:
			finished = true
			t.Fatalf("leader exited before descendant readiness: %v", completed.result.err)
		case <-ctx.Done():
			t.Fatalf("leader did not become ready within startup guard: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := os.Stat(state + ".ready"); err != nil {
		t.Fatalf("real descendant was not started: %v", err)
	}
	// Runtime startup and descendant creation are outside this strict cleanup
	// window. The armed gate releases a real leader exit with inherited pipes.
	started := time.Now()
	if err := os.WriteFile(state+".armed", []byte("armed"), 0o600); err != nil {
		t.Fatal(err)
	}
	var completed completedRun
	select {
	case completed = <-done:
		finished = true
	case <-time.After(1800 * time.Millisecond):
		t.Fatal("leader, descendant, or inherited pipes did not finish within 1800ms of arming")
	}
	r := completed.result
	if ctx.Err() != nil || completed.at.Sub(started) > 1800*time.Millisecond || (r.err != nil && !errors.Is(r.err, execution.ErrUnknownOutcome)) || (r.err == nil && r.output != "leader\n") {
		t.Fatalf("leader/stdio was not bounded after readiness: err=%v ctx=%v elapsed=%v output=%q", r.err, ctx.Err(), completed.at.Sub(started), r.output)
	}
	for _, suffix := range []string{".armed-ready", ".leader-exit"} {
		if _, err := os.Stat(state + suffix); err != nil {
			t.Fatalf("leader or descendant did not reach the real exit phase %s: %v", suffix, err)
		}
	}
	if remaining := time.Until(completed.at.Add(2300 * time.Millisecond)); remaining > 0 {
		time.Sleep(remaining)
	}
	if _, err := os.Stat(state + ".escaped"); !os.IsNotExist(err) {
		t.Fatalf("descendant survived raw command completion: %v", err)
	}
}

type rawLedgerAttempt struct {
	ID       string           `json:"id"`
	Status   execution.Status `json:"result_status"`
	Known    bool             `json:"outcome_known"`
	Readonly bool             `json:"readonly"`
	Stage    string           `json:"stage"`
	TaskID   string           `json:"task_id"`
	Tokens   *int64           `json:"tokens"`
	Cost     *float64         `json:"monetary_cost"`
}

func setupRawStub(t *testing.T, maximum int, mode string) (bin, state, ledger, events string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	state, ledger, events = filepath.Join(dir, "state.txt"), filepath.Join(dir, "ledger.json"), filepath.Join(dir, "events.jsonl")
	for key, value := range map[string]string{
		"MAKEWAND_VERSUS_CHILD_STUB": "1", "MAKEWAND_VERSUS_CHILD_STATE": state, "MAKEWAND_VERSUS_CHILD_MODE": mode,
		"MAKEWAND_CALL_BUDGET_FILE": ledger, "MAKEWAND_MAX_MODEL_CALLS": strconv.Itoa(maximum),
		"MAKEWAND_CALL_BUDGET_LEASE_ID": "", "MAKEWAND_TASK_ID": "versus-offline-test", "MAKEWAND_EXECUTION_EVENTS_FILE": events,
		"MAKEWAND_API_POLICY": "subscription_only", "GORACE": "atexit_sleep_ms=0",
	} {
		t.Setenv(key, value)
	}
	return bin, state, ledger, events
}

func rawStubArgs() []string { return []string{"-test.run=^TestRawCLIChildStub$"} }

func readRawAttempts(t *testing.T, path string) []rawLedgerAttempt {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		Attempts []rawLedgerAttempt `json:"attempts"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	return ledger.Attempts
}

func rawDispatchCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "dispatch\n")
}

func TestRawCLIConcurrentDispatchCannotBypassSharedBudget(t *testing.T) {
	bin, state, ledger, events := setupRawStub(t, 2, "success")
	var eventErrors, successes, budgetRejections atomic.Int32
	startupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx := execution.ContextWithConfig(startupCtx, execution.Config{EventError: func(error) { eventErrors.Add(1) }})
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 30*time.Second)
			switch {
			case r.err == nil:
				successes.Add(1)
			case errors.Is(r.err, execution.ErrBudgetExhausted):
				budgetRejections.Add(1)
			default:
				t.Errorf("raw dispatch: %v", r.err)
			}
		})
	}
	wg.Wait()
	attempts := readRawAttempts(t, ledger)
	if startupCtx.Err() != nil || successes.Load() != 2 || budgetRejections.Load() != 10 || rawDispatchCount(t, state) != 2 || len(attempts) != 2 {
		t.Fatalf("hard limit or successful fixture outcome changed: ctx=%v successes=%d rejected=%d dispatches=%d attempts=%d", startupCtx.Err(), successes.Load(), budgetRejections.Load(), rawDispatchCount(t, state), len(attempts))
	}
	if r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 30*time.Second); !errors.Is(r.err, execution.ErrBudgetExhausted) || rawDispatchCount(t, state) != 2 || len(readRawAttempts(t, ledger)) != 2 {
		t.Fatalf("third real dispatch was not rejected by the same hard budget: %v", r.err)
	}
	attemptIDs := make(map[string]bool)
	for _, attempt := range attempts {
		if attempt.Status != execution.Passed || !attempt.Known || attempt.Readonly || attempt.Stage != "generation" || attempt.TaskID != "versus-offline-test" || attempt.Tokens != nil || attempt.Cost != nil {
			t.Fatalf("incorrect raw metadata: %+v", attempt)
		}
		if !execution.ValidIdentifier(attempt.ID) || attemptIDs[attempt.ID] {
			t.Fatalf("invalid or duplicate raw attempt identity: %q", attempt.ID)
		}
		attemptIDs[attempt.ID] = true
	}
	data, err := os.ReadFile(events)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	spanAttempts := make(map[string]string)
	attemptSpans := make(map[string]string)
	spanKinds := make(map[string]map[string]bool)
	records := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		event, err := execution.DecodeEvent(strings.NewReader(line))
		if err != nil {
			t.Fatal(err)
		}
		if event.Stage != "provider" || event.Readonly || event.TaskID != "versus-offline-test" || event.AttemptID == nil || !attemptIDs[*event.AttemptID] {
			t.Fatalf("incorrect provider event: %+v", event)
		}
		attemptID := *event.AttemptID
		if previous := spanAttempts[event.EventID]; previous != "" && previous != attemptID {
			t.Fatal("raw provider span was shared by different attempts")
		}
		if previous := attemptSpans[attemptID]; previous != "" && previous != event.EventID {
			t.Fatal("raw provider start/end changed event identity")
		}
		spanAttempts[event.EventID], attemptSpans[attemptID] = attemptID, event.EventID
		if spanKinds[event.EventID] == nil {
			spanKinds[event.EventID] = make(map[string]bool)
		}
		if spanKinds[event.EventID][event.Event] {
			t.Fatal("raw provider emitted the same event kind twice")
		}
		spanKinds[event.EventID][event.Event] = true
		if event.Event == "end" && (event.Status == nil || *event.Status != execution.Passed) {
			t.Fatalf("successful raw provider emitted incorrect terminal status: %+v", event)
		}
		records++
	}
	if records+int(eventErrors.Load()) != 4 || strings.Contains(string(data), "synthetic fixture") || strings.Contains(string(data), bin) || strings.Contains(string(data), ledger) {
		t.Fatalf("invalid or leaking raw telemetry: %s", data)
	}
	for _, kinds := range spanKinds {
		if len(kinds) == 2 && (!kinds["start"] || !kinds["end"]) {
			t.Fatal("raw provider pair did not contain one start and one end")
		}
	}
	// Event transport is best effort with a separate 100ms lock guard. Missing
	// records must produce an observable callback, never change the hard budget.
	t.Logf("provider events written=%d recording errors=%d; complete pairs=%v", records, eventErrors.Load(), records == 4)
}

func TestRawCLIEventRecorderFailureKeepsKnownOutcomeAndBudget(t *testing.T) {
	bin, state, ledger, _ := setupRawStub(t, 2, "success")
	// A directory is a deterministic invalid JSONL destination on every OS.
	t.Setenv("MAKEWAND_EXECUTION_EVENTS_FILE", t.TempDir())
	var eventErrors atomic.Int32
	ctx := execution.ContextWithConfig(context.Background(), execution.Config{EventError: func(error) { eventErrors.Add(1) }})
	r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 5*time.Second)
	attempts := readRawAttempts(t, ledger)
	if r.err != nil || eventErrors.Load() != 2 || rawDispatchCount(t, state) != 1 || len(attempts) != 1 || attempts[0].Status != execution.Passed || !attempts[0].Known {
		t.Fatalf("best-effort event failure changed execution or budget: error=%v recording_errors=%d attempts=%+v dispatches=%d", r.err, eventErrors.Load(), attempts, rawDispatchCount(t, state))
	}
}

func TestRawCLIFailureAndUnknownEachConsumeOneAttemptWithoutRetry(t *testing.T) {
	for _, test := range []struct {
		mode   string
		status execution.Status
		known  bool
	}{{"quota", execution.Failed, true}, {"unknown", execution.Unknown, false}} {
		t.Run(test.mode, func(t *testing.T) {
			bin, state, ledger, _ := setupRawStub(t, 3, test.mode)
			r := runCLIWithTimeout(context.Background(), "offline", bin, rawStubArgs(), 3*time.Second)
			attempts := readRawAttempts(t, ledger)
			if r.err == nil || execution.ErrorStatus(r.err) != test.status || rawDispatchCount(t, state) != 1 || len(attempts) != 1 || attempts[0].Status != test.status || attempts[0].Known != test.known {
				t.Fatalf("raw outcome=%v attempts=%+v dispatches=%d", r.err, attempts, rawDispatchCount(t, state))
			}
			if test.mode == "unknown" && !errors.Is(r.err, execution.ErrUnknownOutcome) {
				t.Fatal("uncertain raw failure became replayable")
			}
		})
	}
}

func TestRawCLIHonorsEarlierParentDeadlineAndAccountsTimeout(t *testing.T) {
	bin, state, ledger, _ := setupRawStub(t, 2, "initialization-delay")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	deadline, _ := ctx.Deadline()
	doneObserved := make(chan time.Time, 1)
	stopObserve := context.AfterFunc(ctx, func() { doneObserved <- time.Now() })
	defer stopObserve()
	r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 5*time.Second)
	terminalAt := time.Now()
	var contextDoneAt time.Time
	select {
	case contextDoneAt = <-doneObserved:
	case <-time.After(time.Second):
		t.Fatal("parent deadline completion was not observed")
	}
	attempts := readRawAttempts(t, ledger)
	// The genuine 200ms parent deadline is unchanged. Measure the existing
	// one-second termination bound from its deadline, rather than counting
	// initialization and the wait until the deadline as termination time.
	// The absolute deadline also prevents a delayed observation callback from
	// making a late termination look timely under the race scheduler.
	if !errors.Is(r.err, context.DeadlineExceeded) || !errors.Is(r.err, execution.ErrUnknownOutcome) || terminalAt.Sub(deadline) > time.Second || rawDispatchCount(t, state) != 0 || len(attempts) != 1 || attempts[0].Status != execution.Timeout || attempts[0].Known {
		t.Fatalf("parent deadline not honored: total=%v after_deadline=%v done_observer_delay=%v error=%v attempts=%+v dispatches=%d", terminalAt.Sub(started), terminalAt.Sub(deadline), contextDoneAt.Sub(deadline), r.err, attempts, rawDispatchCount(t, state))
	}
	t.Logf("real parent deadline=200ms total=%v after_deadline=%v done_observer_delay=%v dispatches=0", terminalAt.Sub(started), terminalAt.Sub(deadline), contextDoneAt.Sub(deadline))
}

func TestRawCLIStartFailureHonorsContext(t *testing.T) {
	for _, mode := range []string{"canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			bin, state, _, _ := setupRawStub(t, 1, "success")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			contextErr, status, kind := error(context.Canceled), execution.Cancelled, model.ErrorKindCanceled
			if mode == "deadline" {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				contextErr, status, kind = context.DeadlineExceeded, execution.Timeout, model.ErrorKindTimeout
			}
			p := &rawCLIProvider{name: "offline", path: bin, args: rawStubArgs(), env: os.Environ()}
			// Direct Chat bypasses the outer comparison admission precheck and
			// makes the real exec.CommandContext.Start refuse this context.
			content, usage, err := p.Chat(ctx, nil, "", 0)
			if !errors.Is(err, contextErr) || model.ErrorKindOf(err) != kind || execution.ErrorStatus(err) != status || content != "" || usage.Provider != "offline" || rawDispatchCount(t, state) != 0 {
				t.Fatalf("expired-context Start was misclassified or dispatched: error=%v usage=%+v dispatches=%d", err, usage, rawDispatchCount(t, state))
			}
		})
	}
}

func TestRawCLICancellationKeepsActualDispatchCount(t *testing.T) {
	bin, state, ledger, _ := setupRawStub(t, 2, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type completedRun struct {
		result result
		at     time.Time
	}
	done := make(chan completedRun, 1)
	go func() {
		r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 30*time.Second)
		done <- completedRun{result: r, at: time.Now()}
	}()
	// Wait for the actual child dispatch rather than charging runtime startup
	// against the cancellation cleanup limit. Startup still has a finite guard.
	for {
		if data, err := os.ReadFile(state); err == nil && strings.Contains(string(data), "dispatch\n") {
			break
		}
		select {
		case completed := <-done:
			t.Fatalf("raw command exited before dispatch readiness: %v", completed.result.err)
		case <-ctx.Done():
			t.Fatalf("raw command did not become ready within startup guard: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancelledAt := time.Now()
	cancel()
	var completed completedRun
	select {
	case completed = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("raw process, pipes, or accounting did not finish within two seconds of cancellation")
	}
	r := completed.result
	attempts := readRawAttempts(t, ledger)
	if !errors.Is(r.err, context.Canceled) || !errors.Is(r.err, execution.ErrUnknownOutcome) || completed.at.Sub(cancelledAt) > 2*time.Second || rawDispatchCount(t, state) != 1 || len(attempts) != 1 || attempts[0].Status != execution.Cancelled || attempts[0].Known {
		t.Fatalf("cancellation lost accounting: %v %+v", r.err, attempts)
	}
}

func TestRawCLIPrechecksNeverReserveOrSpawn(t *testing.T) {
	bin, state, ledger, events := setupRawStub(t, 2, "success")
	for _, test := range []struct {
		bin     string
		timeout time.Duration
		args    []string
	}{{filepath.Join(t.TempDir(), "missing.exe"), time.Second, rawStubArgs()}, {bin, 0, rawStubArgs()}, {bin, time.Second, []string{"invalid\x00argument"}}} {
		if r := runCLIWithTimeout(context.Background(), "offline", test.bin, test.args, test.timeout); r.err == nil {
			t.Fatal("invalid raw executable/configuration accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), time.Second); !errors.Is(r.err, context.Canceled) {
		t.Fatal("canceled precheck dispatched a model")
	}
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "") // A maximum alone is fail closed at the library boundary.
	if r := runCLIWithTimeout(context.Background(), "offline", bin, rawStubArgs(), time.Second); r.err == nil {
		t.Fatal("raw arm bypassed maximum without a ledger")
	}
	if rawDispatchCount(t, state) != 0 {
		t.Fatal("precheck failure spawned a model process")
	}
	for _, path := range []string{ledger, events} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("precheck recorded a dispatch: %s: %v", path, err)
		}
	}
}
