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
	"sync/atomic"
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

func TestSharedCapacitySourceMissingStartedTimeKeepsLiveSlot(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	for _, missingAttempt := range []bool{false, true} {
		t.Run(fmt.Sprint(missingAttempt), func(t *testing.T) {
			data := map[string]any{"schema": 1, "maximum": 3, "attempts": []any{map[string]any{"id": "live", "status": "started"}},
				"leases": map[string]any{"live": map[string]any{"keys": []any{"agy:provider", "agy:unknown", "agy:unknown:subscription"}, "owner": leaseProcessRef(os.Getpid()), "workers": []any{}, "launch_pending": false, "expires_at": float64(time.Now().UnixNano())/1e9 + 30}}}
			if missingAttempt {
				data["attempts"] = []any{}
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, fmt.Sprint("legacy-", missingAttempt, ".json"))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			rows, _, _, err := capacitySource(path)
			if err != nil || rows["live"] == nil || rows["live"].StartedAt != 0 {
				t.Fatalf("lost legacy live lease: %v %v", rows, err)
			}
		})
	}
}

func TestSharedCapacityReadyToolWaiterCannotBlockModelPool(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	statePath, _ := capacityPath("shared-capacity.json")
	err := withLockedFile(t.Context(), statePath, nil, func(path *accountingPath) error {
		now := float64(time.Now().UnixNano()) / 1e9
		ticket := &capacityEntry{ID: "tool-waiter", Kind: "tool", Project: "other", Label: "other", Limits: []int{1, 1, 1}, StartedAt: now,
			Lease: map[string]any{"keys": []any{"tool:provider", "tool:unknown", "tool:unknown:subscription"}, "owner": leaseProcessRef(os.Getpid()), "workers": []any{}, "launch_pending": false, "expires_at": now + 30}}
		return saveCapacity(path, &sharedCapacityState{Schema: 1, Active: map[string]*capacityEntry{}, Queue: []*capacityEntry{ticket}})
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := Reserve(ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "calls.json"), TaskID: "model", MaxCalls: 3}), Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedCapacityReapedLegacyFloorAllowsTwoAgySlots(t *testing.T) {
	_ = sharedCapacityFixture(t, 3)
	owner := pythonCommand(t, "import time;time.sleep(20)")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	deadOwner := leaseProcessRef(owner.Process.Pid)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	policy, err := loadCapacityPolicy()
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"attempts": []any{}, "leases": map[string]any{}}
	old, err := admitLeaseWithPolicy(data, "agy", nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	old["owner"], old["expires_at"] = deadOwner, 0
	data["leases"].(map[string]any)["old"] = old
	normalize := func() {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := decodeCapacity(raw, &data); err != nil {
			t.Fatal(err)
		}
	}
	normalize()
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "")
	first, err := admitLeaseWithPolicy(data, "agy", nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	data["leases"].(map[string]any)["first"] = first
	normalize()
	if _, err := admitLeaseWithPolicy(data, "agy", nil, policy); err != nil {
		t.Fatal("reaped old floor poisoned new policy", err)
	}
}

func TestSharedCapacityTildeConfigImportsLegacyDefaultLedger(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	policy := []byte(`{"schema":1,"global_max":3,"project_max":1,"tool_max":2,"wait_seconds":300,"providers":{"codex":1,"agy":2,"claude":1,"fixture":8}}`)
	home := t.TempDir()
	config := filepath.Join(home, "config")
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "capacity-policy.json"), policy, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MAKEWAND_CONFIG_DIR", "~/config")
	now := float64(time.Now().UnixNano()) / 1e9
	data := map[string]any{"schema": 1, "maximum": 3, "attempts": []any{map[string]any{"id": "legacy", "status": "started", "engine": "codex", "started_at": now}},
		"leases": map[string]any{"legacy": map[string]any{"keys": []any{"codex:provider", "codex:unknown", "codex:unknown:subscription"}, "owner": leaseProcessRef(os.Getpid()), "workers": []any{}, "launch_pending": false, "expires_at": now + 30}}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "call-budget.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Reserve(ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "private.json"), TaskID: "private", MaxCalls: 3}), Metadata{Engine: "codex"})
	if !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatal("legacy default ledger was not imported", err)
	}
}

func TestSharedCapacityRecoveryAfterLegacyReaper(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	owner := pythonCommand(t, "import time;time.sleep(20)")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	deadOwner := leaseProcessRef(owner.Process.Pid)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	path := filepath.Join(root, "crashed.json")
	now := float64(time.Now().UnixNano()) / 1e9
	data := map[string]any{"schema": 1, "maximum": 3,
		"attempts": []any{map[string]any{"id": "crashed", "status": "started"}},
		"leases": map[string]any{"crashed": map[string]any{"keys": []any{"codex:provider", "codex:unknown", "codex:unknown:subscription"},
			"owner": deadOwner, "workers": []any{}, "launch_pending": false, "expires_at": now + 30}}}
	write := func() {
		t.Helper()
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	rows, _, _, err := capacitySource(path)
	if err != nil || rows["crashed"] == nil {
		t.Fatalf("cache fixture: %v %v", rows, err)
	}
	row := rows["crashed"]
	state := &sharedCapacityState{Schema: 1, Active: map[string]*capacityEntry{row.ID: row}}
	delete(data["leases"].(map[string]any), "crashed")
	write() // old reapers remove the lease but keep the charged attempt
	row.Lease["expires_at"] = json.Number("0")
	row.Lease["owner"] = leaseProcessRef(os.Getpid())
	refreshCapacity(state, path)
	if state.Active[row.ID] == nil {
		t.Fatal("released live cached owner")
	}
	row.Lease["owner"] = deadOwner
	row.Lease["workers"] = []any{leaseProcessRef(os.Getpid())}
	refreshCapacity(state, path)
	if state.Active[row.ID] == nil {
		t.Fatal("released live cached worker")
	}
	row.Lease["workers"] = []any{}
	entries := data["attempts"]
	data["attempts"] = []any{}
	write()
	refreshCapacity(state, path)
	if state.Active[row.ID] == nil {
		t.Fatal("reset source released an unproven cached slot")
	}
	data["attempts"] = entries
	write()
	refreshCapacity(state, path)
	if state.Active[row.ID] != nil {
		t.Fatal("dead cached slot survived legacy reaping")
	}

	t.Run("source clock", func(t *testing.T) {
		data["leases"] = map[string]any{"crashed": row.Lease}
		row.Lease["expires_at"] = now + 5
		data["scopes"] = map[string]any{"clock": now + 10, "day": "", "tasks": map[string]any{}, "projects": map[string]any{}, "machine": map[string]any{}}
		write()
		rows, _, _, err := capacitySource(path)
		if err != nil || len(rows) != 0 {
			t.Fatalf("source and reaper clock disagree: %v %v", rows, err)
		}
	})

	t.Run("persistent reaping evidence", func(t *testing.T) {
		if err := readCapacityFile(path, &data); err != nil {
			t.Fatal(err)
		}
		if _, err := admitLease(data, "agy", nil); err != nil {
			t.Fatal(err)
		}
		entry := data["attempts"].([]any)[0].(map[string]any)
		if entry["lease_reaped"] != true || entry["status"] != "started" {
			t.Fatalf("lost recovery evidence or refunded call: %v", entry)
		}
		write()
		// The positive source marker also settles an older cache snapshot.
		row.Lease["owner"] = leaseProcessRef(os.Getpid())
		row.Lease["expires_at"] = json.Number(fmt.Sprint(now + 30))
		state.Active[row.ID] = row
		refreshCapacity(state, path)
		if state.Active[row.ID] != nil {
			t.Fatal("source reaping evidence did not settle cached slot")
		}
	})
}

func TestSharedCapacityInvalidSourceCannotReleaseCachedLiveSlot(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	for _, field := range []string{"schema", "maximum", "scopes"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(root, field+".json")
			data := map[string]any{"schema": 1, "maximum": 3,
				"attempts": []any{map[string]any{"id": "live", "status": "started"}},
				"leases": map[string]any{"live": map[string]any{"keys": []any{"codex:provider", "codex:unknown", "codex:unknown:subscription"},
					"owner": leaseProcessRef(os.Getpid()), "workers": []any{}, "launch_pending": false, "expires_at": float64(time.Now().UnixNano())/1e9 + 30}}}
			write := func() {
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write()
			rows, _, _, err := capacitySource(path)
			if err != nil || rows["live"] == nil {
				t.Fatal("invalid live fixture", err)
			}
			row := rows["live"]
			state := &sharedCapacityState{Schema: 1, Active: map[string]*capacityEntry{row.ID: row}}
			data["leases"] = map[string]any{}
			data["attempts"].([]any)[0].(map[string]any)["status"] = "completed"
			original, existed := data[field]
			data[field] = map[string]any{"schema": 2, "maximum": 0, "scopes": map[string]any{"clock": "broken"}}[field]
			write()
			if _, _, _, err := capacitySource(path); err == nil {
				t.Error("invalid source accepted as settlement evidence")
			}
			refreshCapacity(state, path)
			if state.Active[row.ID] == nil {
				t.Error("invalid source released a live cached slot")
			}
			if existed {
				data[field] = original
			} else {
				delete(data, field)
			}
			// Valid historical evidence must survive the source reader even
			// when normal budget loading would compact this old completion.
			data["attempts"].([]any)[0].(map[string]any)["finished_at"] = float64(time.Now().Add(-72*time.Hour).UnixNano()) / 1e9
			write()
			refreshCapacity(state, path)
			if state.Active[row.ID] != nil {
				t.Error("valid historical settlement evidence was lost")
			}
		})
	}
}

func TestSharedCapacityIdleCompletionSurvivesInitializeBudget(t *testing.T) {
	root := sharedCapacityFixture(t, 3)
	path := filepath.Join(root, "idle.json")
	cfg := Config{LedgerPath: path, TaskID: "idle", MaxCalls: 3}
	ctx := ContextWithConfig(t.Context(), cfg)
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := readCapacityFile(path, &data); err != nil {
		t.Fatal(err)
	}
	data["attempts"].([]any)[0].(map[string]any)["finished_at"] = float64(time.Now().Add(-49*time.Hour).UnixNano()) / 1e9
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeBudget(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := readCapacityFile(path, &data); err != nil {
		t.Fatal(err)
	}
	if len(data["attempts"].([]any)) != 1 {
		t.Fatal("compaction removed the only cached settlement evidence")
	}
	// The Python writer must preserve the same Go receipt before admission.
	cmd := pythonCommand(t, `import os
from makewand import call_budget as b
b.request_budget('idle',maximum=3,budget_file=os.environ['FIXTURE_LEDGER'])`)
	cmd.Env = append(os.Environ(), "FIXTURE_LEDGER="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python compaction: %v %s", err, out)
	}
	other := ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "next.json"), TaskID: "next", MaxCalls: 3})
	b, err := Reserve(other, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal("idle completion permanently occupied Codex", err)
	}
	if err := b.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if err := InitializeBudget(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := readCapacityFile(path, &data); err != nil {
		t.Fatal(err)
	}
	if len(data["attempts"].([]any)) != 0 {
		t.Fatal("unreferenced old receipt never compacted")
	}
}

func TestSharedCapacityRegistrationFailureReleasesUninvokedReservation(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			root := sharedCapacityFixture(t, 3)
			original := accountingSyncDirectory
			injected := errors.New("injected shared registration sync failure")
			var fail atomic.Bool
			fail.Store(true)
			accountingSyncDirectory = func(path *accountingPath) error {
				if err := original(path); err != nil {
					return err
				}
				if path.name == "shared-capacity.json" && fail.CompareAndSwap(true, false) {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { accountingSyncDirectory = original })
			path := filepath.Join(root, "failed.json")
			ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "failed", MaxCalls: 3})
			if a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: pending}); a != nil || !errors.Is(err, injected) {
				t.Fatalf("registration returned a usable reservation: %v %v", a, err)
			}
			var data map[string]any
			if err := readCapacityFile(path, &data); err != nil {
				t.Fatal(err)
			}
			if len(data["leases"].(map[string]any)) != 0 {
				t.Fatal("live caller retained an uninvoked lease")
			}
			if data["attempts"].([]any)[0].(map[string]any)["status"] != "released" {
				t.Fatal("failed registration never settled")
			}
			counter := data["scopes"].(map[string]any)["tasks"].(map[string]any)["failed"].(map[string]any)
			if used, _ := jsonInteger(counter["used"]); used != 0 {
				t.Fatal("never-dispatched registration consumed a call")
			}
			other := ContextWithConfig(t.Context(), Config{LedgerPath: filepath.Join(root, "next.json"), TaskID: "next", MaxCalls: 3})
			b, err := Reserve(other, Metadata{Engine: "codex"})
			if err != nil {
				t.Fatal("failed registration permanently occupied provider", err)
			}
			if err := b.Complete(Outcome{Status: Passed, Known: true}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
