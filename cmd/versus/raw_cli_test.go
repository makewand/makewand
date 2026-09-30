package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
)

// This test executable is the only child process used by the raw-arm tests.
// It avoids vendor CLIs, network access, and POSIX-only script fixtures.
func TestRawCLIChildStub(t *testing.T) {
	if os.Getenv("MAKEWAND_VERSUS_CHILD_STUB") != "1" {
		return
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
		time.Sleep(5 * time.Second)
	default:
		fmt.Println("--- FILE: fixture.txt ---\n```\nsynthetic fixture\n```")
	}
	os.Exit(0)
}

type rawLedgerAttempt struct {
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
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			r := runCLIWithTimeout(context.Background(), "offline", bin, rawStubArgs(), 3*time.Second)
			if r.err != nil && !errors.Is(r.err, execution.ErrBudgetExhausted) {
				t.Errorf("raw dispatch: %v", r.err)
			}
		})
	}
	wg.Wait()
	attempts := readRawAttempts(t, ledger)
	if rawDispatchCount(t, state) != 2 || len(attempts) != 2 {
		t.Fatalf("hard limit bypass: dispatches=%d attempts=%d", rawDispatchCount(t, state), len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Status != execution.Passed || !attempt.Known || attempt.Readonly || attempt.Stage != "generation" || attempt.TaskID != "versus-offline-test" || attempt.Tokens != nil || attempt.Cost != nil {
			t.Fatalf("incorrect raw metadata: %+v", attempt)
		}
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		event, err := execution.DecodeEvent(strings.NewReader(line))
		if err != nil {
			t.Fatal(err)
		}
		if event.Stage != "provider" || event.Readonly || event.TaskID != "versus-offline-test" {
			t.Fatalf("incorrect provider event: %+v", event)
		}
		seen[event.EventID]++
	}
	if len(seen) != 2 || strings.Contains(string(data), "synthetic fixture") || strings.Contains(string(data), bin) || strings.Contains(string(data), ledger) {
		t.Fatalf("invalid or leaking raw telemetry: %s", data)
	}
	for _, paired := range seen {
		if paired != 2 {
			t.Fatal("raw provider start/end did not share event identity")
		}
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
	bin, state, ledger, _ := setupRawStub(t, 2, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 5*time.Second)
	attempts := readRawAttempts(t, ledger)
	if !errors.Is(r.err, context.DeadlineExceeded) || !errors.Is(r.err, execution.ErrUnknownOutcome) || time.Since(started) > time.Second || rawDispatchCount(t, state) != 1 || len(attempts) != 1 || attempts[0].Status != execution.Timeout || attempts[0].Known {
		t.Fatalf("parent deadline not honored: elapsed=%v error=%v attempts=%+v dispatches=%d", time.Since(started), r.err, attempts, rawDispatchCount(t, state))
	}
}

func TestRawCLICancellationKeepsActualDispatchCount(t *testing.T) {
	bin, state, ledger, _ := setupRawStub(t, 2, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(state); err == nil && strings.Contains(string(data), "dispatch\n") {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	r := runCLIWithTimeout(ctx, "offline", bin, rawStubArgs(), 5*time.Second)
	<-done
	attempts := readRawAttempts(t, ledger)
	if !errors.Is(r.err, context.Canceled) || !errors.Is(r.err, execution.ErrUnknownOutcome) || rawDispatchCount(t, state) != 1 || len(attempts) != 1 || attempts[0].Status != execution.Cancelled || attempts[0].Known {
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
