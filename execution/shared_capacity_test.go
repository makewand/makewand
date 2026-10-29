package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sharedCapacityFixture(t *testing.T, maximum int) string {
	t.Helper()
	_ = layeredFixture(t)
	root := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", root)
	t.Setenv("MAKEWAND_TASK_ID", "")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "3")
	for _, name := range []string{"MAKEWAND_ACCOUNT_CONCURRENCY", "MAKEWAND_POOL_CONCURRENCY", "MAKEWAND_PROVIDER_CONCURRENCY"} {
		t.Setenv(name, "")
	}
	policy := hostCapacityPolicy{Schema: 1, GlobalMax: maximum, ProjectMax: 1, ToolMax: 2, WaitSeconds: 300, Providers: map[string]int{"codex": 1, "agy": 2, "claude": 1, "fixture": 8}}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "capacity-policy.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSharedCapacityPrivateLedgersAndPythonParity(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	firstPath := filepath.Join(root, "python.json")
	cmd := pythonCommand(t, `import os
from makewand import call_budget as b
b.reserve('codex','fast',task_id='python-task',budget_file=os.environ['FIXTURE_LEDGER'])`)
	cmd.Env = append(os.Environ(), "FIXTURE_LEDGER="+firstPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python: %v %s", err, out)
	}
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "go.json"), TaskID: "go-task", MaxCalls: 3})
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("Codex private-ledger bypass: %v", err)
	}
	a, err := Reserve(ctx, Metadata{Engine: "agy"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Complete(Outcome{Status: Passed, Known: true}) })
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	cmd = pythonCommand(t, `import os
from makewand import call_budget as b
a=b.reserve('gemini','fast',task_id='second-agy',budget_file=os.environ['FIXTURE_LEDGER'])
try: b.reserve('agy','fast',task_id='third-agy',budget_file=os.environ['FIXTURE_LEDGER']+'.other')
except b.ConcurrencyBusyError: pass
else: raise RuntimeError('global capacity bypassed')
b.complete(a,True,0,budget_file=os.environ['FIXTURE_LEDGER'])`)
	cmd.Env = append(os.Environ(), "FIXTURE_LEDGER="+filepath.Join(root, "second-python.json"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "second-python.json.other")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("busy attempt created call ledger", err)
	}
}

func TestSharedCapacityProcessHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_CAPACITY_TEST_HELPER") != "1" {
		return
	}
	ctx := ContextWithConfig(t.Context(), Config{TaskID: fmt.Sprint(os.Getpid()), MaxCalls: 3})
	_, err := Reserve(ctx, Metadata{Engine: "fixture"})
	if errors.Is(err, ErrConcurrencyBusy) {
		fmt.Println("CAPACITY_REFUSED")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("CAPACITY_ADMITTED")
}

func TestSharedCapacityMixedProcessesAcrossDifferentLedgers(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "5")
	commands := []*exec.Cmd{}
	for i := range 8 {
		var cmd *exec.Cmd
		if i%2 == 0 {
			//nolint:gosec // Fixed self-test selector; no model or external executable.
			cmd = exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSharedCapacityProcessHelper$")
		} else {
			cmd = pythonCommand(t, `import os
from makewand import call_budget as b
try: b.reserve('fixture','fast',task_id=str(os.getpid()))
except b.ConcurrencyBusyError: print('CAPACITY_REFUSED')
else: print('CAPACITY_ADMITTED')`)
		}
		project := filepath.Join(root, fmt.Sprint("project-", i))
		if err := os.Mkdir(project, 0700); err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(os.Environ(), "MAKEWAND_CAPACITY_TEST_HELPER=1", "MAKEWAND_CALL_BUDGET_FILE="+filepath.Join(root, fmt.Sprint("calls-", i, ".json")), "MAKEWAND_PROJECT_ROOT="+project)
		commands = append(commands, cmd)
	}
	var wait sync.WaitGroup
	results := make(chan string, len(commands))
	for _, cmd := range commands {
		wait.Add(1)
		go func() {
			defer wait.Done()
			out, err := cmd.CombinedOutput()
			if err != nil {
				results <- fmt.Sprintf("ERROR %v %s", err, out)
			} else {
				results <- string(out)
			}
		}()
	}
	wait.Wait()
	close(results)
	admitted, refused := 0, 0
	for out := range results {
		if strings.Contains(out, "ERROR") {
			t.Error(out)
		}
		admitted += strings.Count(out, "CAPACITY_ADMITTED")
		refused += strings.Count(out, "CAPACITY_REFUSED")
	}
	if admitted != 3 || refused != 5 {
		t.Fatalf("admitted=%d refused=%d", admitted, refused)
	}
	statePath, _ := capacityPath("shared-capacity.json")
	var state sharedCapacityState
	if err := readCapacityFile(statePath, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Active) != 3 || len(state.Queue) != 0 {
		t.Fatalf("active=%d queue=%d", len(state.Active), len(state.Queue))
	}
}

func TestSharedCapacityWaitLeavesExecutionBudgetAndNoCharge(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "first.json"), TaskID: "first", MaxCalls: 3})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Complete(Outcome{Status: Passed, Known: true}) })
	t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "300")
	waitCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	path := filepath.Join(root, "waiting.json")
	start := time.Now()
	_, err = Reserve(ContextWithConfig(waitCtx, Config{LedgerPath: path, TaskID: "waiting", MaxCalls: 3}), Metadata{Engine: "codex"})
	if !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatal(err)
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Fatal("queue consumed execution budget")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("queued call was charged", err)
	}
}

func TestSharedCapacityTargetWorkingDirectoryAndGitWorktreeParity(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	t.Setenv("MAKEWAND_PROJECT_ROOT", "")
	repository := filepath.Join(root, "main")
	worktree := filepath.Join(root, "linked")
	metadata := filepath.Join(repository, ".git", "worktrees", "linked")
	if err := os.MkdirAll(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "commondir"), []byte("../..\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+metadata+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mainID, _, err := capacityProject(repository)
	if err != nil {
		t.Fatal(err)
	}
	linkedID, _, err := capacityProject(worktree)
	if err != nil || mainID != linkedID {
		t.Fatalf("%s != %s: %v", mainID, linkedID, err)
	}
	cmd := pythonCommand(t, `from makewand import shared_capacity as c; print(c.project(sys.argv[2])[0])`, worktree)
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != mainID {
		t.Fatalf("Python project identity: %s %v", out, err)
	}
	first, err := Reserve(ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "main.json"), TaskID: "first", MaxCalls: 3}), Metadata{Engine: "codex", WorkingDir: repository})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Complete(Outcome{Status: Passed, Known: true}) })
	_, err = Reserve(ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "linked.json"), TaskID: "second", MaxCalls: 3}), Metadata{Engine: "agy", WorkingDir: worktree})
	if !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatal("worktree project capacity bypass", err)
	}
}
