package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/processjob"
)

func readTestLedger(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger map[string]any
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	return ledger
}

func TestLedgerConcurrentHardLimitAndUnknownMeasurements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 7, TaskID: "concurrent-test"})
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 60 {
		wg.Go(func() {
			attempt, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"})
			if errors.Is(err, ErrBudgetExhausted) {
				return
			}
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			admitted.Add(1)
			if err := attempt.Complete(Outcome{Status: Unknown, Known: false}); err != nil {
				t.Errorf("complete: %v", err)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 7 {
		t.Fatalf("admitted=%d, want 7", admitted.Load())
	}
	ledger := readTestLedger(t, path)
	for _, raw := range ledger["attempts"].([]any) {
		entry := raw.(map[string]any)
		if entry["status"] != "completed" || entry["result_status"] != "UNKNOWN" || entry["outcome_known"] != false || entry["tokens"] != nil || entry["monetary_cost"] != nil {
			t.Fatalf("unknown result misreported: %+v", entry)
		}
	}
}

func TestLedgerMaximumCannotBeRaisedAfterRejectedLowerLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 5})
	for range 2 {
		if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); err != nil {
			t.Fatal(err)
		}
	}
	lower := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1})
	if _, err := Reserve(lower, Metadata{Engine: "synthetic", Tier: "standard"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("lower maximum: %v", err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("maximum raised: %v", err)
	}
	if readTestLedger(t, path)["maximum"] != float64(1) {
		t.Fatal("rejected lower maximum did not persist")
	}
}

func TestContextCannotDropInheritedBudgetOrSwitchLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent.json")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "1")
	ctx := ContextWithConfig(context.Background(), Config{})
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("empty config dropped parent cap: %v", err)
	}
	other := ContextWithConfig(context.Background(), Config{LedgerPath: filepath.Join(t.TempDir(), "escape.json"), MaxCalls: 20})
	if _, err := Reserve(other, Metadata{Engine: "synthetic", Tier: "standard"}); err == nil {
		t.Fatal("different ledger escaped inherited budget")
	}
	largerContext, cfg, err := EnsureContext(ContextWithConfig(context.Background(), Config{MaxCalls: 20}))
	if err != nil || cfg.MaxCalls != 1 {
		t.Fatalf("larger explicit cap relaxed inheritance: %+v %v", cfg, err)
	}
	// EvalSymlinks expands Windows 8.3 ancestors, so preserved ledger identity
	// must be checked on the actual file rather than its original spelling.
	inherited, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := os.Stat(cfg.LedgerPath)
	if err != nil || !os.SameFile(inherited, resolved) {
		t.Fatalf("larger explicit cap switched ledger identity: %q -> %q, %v", path, cfg.LedgerPath, err)
	}
	if _, err := Reserve(largerContext, Metadata{Engine: "synthetic", Tier: "standard"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("larger explicit cap admitted a call past the inherited maximum: %v", err)
	}
}

func TestNestedContextCannotRelaxParentBudgetWithoutEnvironment(t *testing.T) {
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
	path := filepath.Join(t.TempDir(), "parent.json")
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1})
	ctx = ContextWithConfig(ctx, Config{})
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("nested config dropped cap: %v", err)
	}
	otherPath := filepath.Join(t.TempDir(), "other.json")
	escape := ContextWithConfig(ctx, Config{LedgerPath: otherPath, MaxCalls: 20})
	escape = ContextWithConfig(escape, Config{LedgerPath: otherPath, MaxCalls: 20})
	if _, err := Reserve(escape, Metadata{Engine: "synthetic", Tier: "standard"}); err == nil {
		t.Fatal("nested context replaced original ledger")
	}
}

func TestLedgerMalformedStateFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{"schema":true,"maximum":3,"attempts":[]}`,
		`{"schema":1,"maximum":true,"attempts":[]}`,
		`{"schema":1,"maximum":3,"attempts":[{"id":"x"},{"id":"x"}]}`,
		`{"schema":1,"maximum":3,"attempts":[],"holds":{"x":{"remaining":true,"expires_at":9999999999}}}`,
		`{"schema":1,"maximum":3,"attempts":[]} {}`,
	} {
		path := filepath.Join(t.TempDir(), "calls.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 3})
		if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); err == nil || errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("malformed ledger admitted: %s; %v", body, err)
		}
	}
	ctx := ContextWithConfig(context.Background(), Config{MaxCalls: 1})
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); err == nil {
		t.Fatal("library maximum without persistent path accepted")
	}
}

func TestLedgerRejectsOversizedFileBeforeDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maximumLedgerBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 1})
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("oversized ledger admitted: %v", err)
	}
}

func pythonCommand(t *testing.T, code string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return pythonCommandWithContext(t, ctx, code, args...)
}

func pythonCommandWithContext(t *testing.T, ctx context.Context, code string, args ...string) *exec.Cmd {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 unavailable")
	}
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"-I", "-c", "import sys; sys.path.insert(0,sys.argv[1]); " + code, repo}, args...)
	// #nosec G204 G702 -- executable is the local Python interpreter; program text is fixed by this synthetic test, never model/user code.
	return exec.CommandContext(ctx, python, argv...)
}

func TestLedgerProcessHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_LEDGER_TEST_HELPER") != "1" {
		return
	}
	admissionCtx := t.Context()
	if barrier := os.Getenv("MAKEWAND_LEDGER_TEST_BARRIER"); barrier != "" {
		if ready := os.Getenv("MAKEWAND_LEDGER_TEST_READY"); ready != "" {
			// #nosec G703 -- parent-created fixture handshake path, never external input.
			if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		for {
			// #nosec G703 -- barrier is a parent test-created temporary synchronization file inherited only by this fixed self-test helper.
			if _, err := os.Stat(barrier); err == nil {
				break
			}
			if t.Context().Err() != nil {
				t.Fatal(t.Context().Err())
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Both languages derive their admission context from this same release
		// deadline. Completion still uses its independent production 5s bound.
		deadlinePath := os.Getenv("MAKEWAND_LEDGER_TEST_DEADLINE")
		// #nosec G703 -- parent-created deadline file inherited only by this owned helper.
		deadlineBytes, err := os.ReadFile(deadlinePath)
		if err != nil {
			t.Fatal(err)
		}
		deadlineMS, err := strconv.ParseInt(strings.TrimSpace(string(deadlineBytes)), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		var stopAdmission context.CancelFunc
		admissionCtx, stopAdmission = context.WithDeadline(t.Context(), time.UnixMilli(deadlineMS))
		defer stopAdmission()
	}
	workerStarted := time.Now()
	var maxReserve, maxComplete time.Duration
	admitted, exhausted := 0, 0
	for step := range 20 {
		reserveStarted := time.Now()
		attempt, err := Reserve(admissionCtx, Metadata{Engine: "go-synthetic", Tier: "standard"})
		reserveElapsed := time.Since(reserveStarted)
		maxReserve = max(maxReserve, reserveElapsed)
		if errors.Is(err, ErrBudgetExhausted) {
			exhausted++
			// All competing fixture attempts yield equally, including rejections;
			// the test asserts shared accounting, rather than hot-loop throughput.
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("worker reserve step=%d elapsed=%v workflow_elapsed=%v context=%v: %v", step, reserveElapsed, time.Since(workerStarted), admissionCtx.Err(), err)
		}
		admitted++
		completeStarted := time.Now()
		if err := attempt.Complete(Outcome{Status: Unknown, Known: false}); err != nil {
			t.Fatalf("worker complete step=%d elapsed=%v workflow_elapsed=%v: %v", step, time.Since(completeStarted), time.Since(workerStarted), err)
		}
		maxComplete = max(maxComplete, time.Since(completeStarted))
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Printf("mixed-ledger-worker operations=20 admitted=%d exhausted=%d max_reserve=%v max_complete=%v workflow_elapsed=%v\n", admitted, exhausted, maxReserve, maxComplete, time.Since(workerStarted))
}

func TestMixedGoPythonProcessesShareHardLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "13")
	t.Setenv("MAKEWAND_TASK_ID", "mixed-process-test")
	t.Setenv("MAKEWAND_EXECUTION_EVENTS_FILE", "")
	t.Setenv("MAKEWAND_CALL_BUDGET_LEASE_ID", "")
	initialCtx, stopInitial := context.WithTimeout(t.Context(), 30*time.Second)
	defer stopInitial()
	initial, err := Reserve(initialCtx, Metadata{Engine: "go-synthetic", Tier: "standard"})
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Complete(Outcome{Status: Unknown, Known: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := pythonCommand(t, "import time; from makewand import call_budget as b; a=b.reserve('python-synthetic','standard',deadline_monotonic=time.monotonic()+30); b.complete(a,False,0,result_status='UNKNOWN',outcome_known=False)").Output(); err != nil {
		t.Fatal(err)
	}
	var commands []*exec.Cmd
	barrier := filepath.Join(t.TempDir(), "start")
	deadlinePath := filepath.Join(filepath.Dir(barrier), "deadline-unix-ms")
	pythonWorker := `import json,os,time
from makewand import call_budget as b
with open(sys.argv[3],'w') as ready: ready.write('ready')
while not os.path.exists(sys.argv[2]): time.sleep(.005)
with open(sys.argv[4]) as deadline_file: deadline_unix_ms=int(deadline_file.read())
deadline=time.monotonic()+max(0,deadline_unix_ms/1000-time.time())
started=time.monotonic()
admitted=exhausted=0
max_reserve=max_complete=0
for step in range(20):
 reserve_started=time.monotonic()
 try:
  a=b.reserve('python-synthetic','standard',deadline_monotonic=deadline)
 except b.BudgetError as error:
  reserve_elapsed=time.monotonic()-reserve_started
  if not str(error).startswith('model task budget exhausted ('):
   raise RuntimeError(f'worker reserve step={step} elapsed={time.monotonic()-reserve_started:.6f} workflow_elapsed={time.monotonic()-started:.6f} deadline_remaining={deadline-time.monotonic():.6f}') from error
  exhausted+=1
 else:
  reserve_elapsed=time.monotonic()-reserve_started
  admitted+=1
  complete_started=time.monotonic()
  try:
   b.complete(a,False,0,result_status='UNKNOWN',outcome_known=False)
  except Exception as error:
   raise RuntimeError(f'worker complete step={step} elapsed={time.monotonic()-complete_started:.6f} workflow_elapsed={time.monotonic()-started:.6f}') from error
  max_complete=max(max_complete,time.monotonic()-complete_started)
 max_reserve=max(max_reserve,reserve_elapsed)
 time.sleep(.005)
print('mixed-ledger-worker '+json.dumps(dict(operations=20,admitted=admitted,exhausted=exhausted,max_reserve_seconds=max_reserve,max_complete_seconds=max_complete,workflow_elapsed_seconds=time.monotonic()-started)),flush=True)
`
	// This bounds initialization plus 160 competing ledger operations. It is
	// a fixture guard, not a model-call deadline or a throughput requirement.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var readyFiles []string
	for i := range 4 {
		goReady := filepath.Join(filepath.Dir(barrier), fmt.Sprintf("go-%d.ready", i))
		pythonReady := filepath.Join(filepath.Dir(barrier), fmt.Sprintf("python-%d.ready", i))
		// #nosec G204 G702 -- runs this Go test binary with a fixed helper test, never an arbitrary repository/model executable.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLedgerProcessHelper$")
		cmd.Env = append(os.Environ(), "MAKEWAND_LEDGER_TEST_HELPER=1", "MAKEWAND_LEDGER_TEST_BARRIER="+barrier, "MAKEWAND_LEDGER_TEST_READY="+goReady, "MAKEWAND_LEDGER_TEST_DEADLINE="+deadlinePath)
		commands = append(commands, cmd, pythonCommandWithContext(t, ctx, pythonWorker, barrier, pythonReady, deadlinePath))
		readyFiles = append(readyFiles, goReady, pythonReady)
	}
	var diagnostics []*processjob.Capture
	startedCommands := 0
	waited := make(map[*exec.Cmd]bool)
	defer func() {
		cancel()
		for _, cmd := range commands[:startedCommands] {
			if !waited[cmd] {
				_ = cmd.Wait()
			}
		}
	}()
	for _, cmd := range commands {
		capture := processjob.NewCapture(16<<10, nil)
		cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
		diagnostics = append(diagnostics, capture)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		startedCommands++
	}
	readyCtx, stopReady := context.WithTimeout(ctx, 30*time.Second)
	defer stopReady()
	for i, ready := range readyFiles {
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			select {
			case <-readyCtx.Done():
				t.Fatalf("worker %d did not reach the shared start barrier: %v stdout=%q stderr=%q", i, readyCtx.Err(), diagnostics[i].StdoutString(), diagnostics[i].StderrString())
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	stopReady()
	// The 160 operations share an actual caller deadline starting after all
	// eight workers are ready; this is separate from the 30s initialization
	// and 2min outer process guards. No worker is serialized by the fixture.
	admissionDeadline := time.Now().Add(30 * time.Second)
	if err := os.WriteFile(deadlinePath, []byte(strconv.FormatInt(admissionDeadline.UnixMilli(), 10)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(barrier, []byte("start"), 0600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range commands {
		err := cmd.Wait()
		waited[cmd] = true
		t.Logf("worker %d finished with error=%v guard=%v stdout=%q stderr=%q", i, err, ctx.Err(), diagnostics[i].StdoutString(), diagnostics[i].StderrString())
		if err != nil {
			t.Fatalf("worker %d failed: %v guard=%v stdout=%q stderr=%q", i, err, ctx.Err(), diagnostics[i].StdoutString(), diagnostics[i].StderrString())
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("mixed-language workload depended on the fixture guard: %v", ctx.Err())
	}
	ledger := readTestLedger(t, path)
	if attempts := ledger["attempts"].([]any); len(attempts) != 13 {
		t.Fatalf("mixed-process attempts=%d, want13", len(attempts))
	}
	engines := make(map[string]bool)
	for _, raw := range ledger["attempts"].([]any) {
		entry := raw.(map[string]any)
		engines[entry["engine"].(string)] = true
		if entry["status"] != "completed" || entry["result_status"] != "UNKNOWN" || entry["outcome_known"] != false {
			t.Fatalf("mixed-language attempt lost its terminal outcome: %+v", entry)
		}
	}
	if !engines["go-synthetic"] || !engines["python-synthetic"] {
		t.Fatalf("mixed test did not dispatch both languages: %v", engines)
	}
}

func TestPythonJudgeHoldCannotBeConsumedByGoRegularCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "3")
	t.Setenv("MAKEWAND_EXECUTION_EVENTS_FILE", "")
	t.Setenv("MAKEWAND_CALL_BUDGET_LEASE_ID", "")
	output, err := pythonCommand(t, "from makewand import call_budget as b; print(b.reserve_capacity(2,'judge-task','review',60))").Output()
	if err != nil {
		t.Fatal(err)
	}
	lease := strings.TrimSpace(string(output))
	ctx := context.Background()
	if _, err := Reserve(ctx, Metadata{Engine: "go-candidate", Tier: "standard"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "go-candidate", Tier: "standard"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("judge capacity stolen: %v", err)
	}
	claim := ContextWithConfig(ctx, Config{LedgerPath: path, MaxCalls: 3, LeaseID: lease})
	if _, err := Reserve(claim, Metadata{Engine: "go-judge", Tier: "standard", Stage: "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pythonCommand(t, "from makewand import call_budget as b; a=b.reserve('python-judge','standard',lease_id=sys.argv[2],stage='review'); b.complete(a,True,0,result_status='PASSED')", lease).Output(); err != nil {
		t.Fatal(err)
	}
	ledger := readTestLedger(t, path)
	if len(ledger["attempts"].([]any)) != 3 {
		t.Fatal("lease claims not counted as actual calls")
	}
	if ledger["holds"].(map[string]any)[lease].(map[string]any)["remaining"] != float64(0) {
		t.Fatal("lease capacity not consumed")
	}
}

func TestEventRecorderStagesDoNotConsumeCallsOrLeakContent(t *testing.T) {
	dir := t.TempDir()
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: filepath.Join(dir, "calls.json"), MaxCalls: 1, EventsPath: filepath.Join(dir, "events.jsonl"), TaskID: "events-task"})
	span, err := StartStageForAttempt(ctx, "copy", "go", false, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := span.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls.json")); !os.IsNotExist(err) {
		t.Fatalf("non-provider stage consumed ledger: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"prompt", "cwd", "output", "error", "credential"} {
			if _, exists := entry[forbidden]; exists {
				t.Fatalf("event contains %s", forbidden)
			}
		}
		if entry["task_id"] != "events-task" || entry["attempt_id"] != "candidate-1" {
			t.Fatalf("stage identity changed: %+v", entry)
		}
	}
}
