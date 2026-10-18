package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureAccount = "codex:account:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestLeaseExpiryCannotReleaseLiveOwner(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	account := fixtureAccount
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, TaskID: "lease", MaxCalls: 10, AccountRef: &account})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	err = withLockedFile(t.Context(), path, nil, func(p *accountingPath) error {
		data, err := readLedger(p, 10)
		if err != nil {
			return err
		}
		data["leases"].(map[string]any)[a.ID].(map[string]any)["expires_at"] = 0
		return saveLedger(p, data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatal(err)
	}
	if err := a.updateLease("heartbeat", 0); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Unknown, Known: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); err != nil {
		t.Fatal(err)
	}
}
func TestLeaseProcessHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_LEASE_TEST_HELPER") != "1" {
		return
	}
	account := fixtureAccount
	ctx := ContextWithConfig(t.Context(), Config{AccountRef: &account, TaskID: NewID()})
	_, err := Reserve(ctx, Metadata{Engine: "codex"})
	if errors.Is(err, ErrConcurrencyBusy) {
		fmt.Println("LEASE_REFUSED")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("LEASE_ADMITTED")
}
func TestMixedGoPythonProcessesCompeteForLastAccountSlot(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "100")
	t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
	var commands []*exec.Cmd
	for range 4 {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLeaseProcessHelper$")
		cmd.Env = append(os.Environ(), "MAKEWAND_LEASE_TEST_HELPER=1")
		commands = append(commands, cmd, pythonCommand(t, `import os
from makewand import call_budget as b
try: b.reserve('codex','fast',account_ref='codex:account:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',task_id=str(os.getpid()))
except b.BudgetError as e:
 if 'concurrency' not in str(e): raise
 print('LEASE_REFUSED')
else: print('LEASE_ADMITTED')`))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for _, cmd := range commands {
		wg.Go(func() {
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("helper: %v %s", err, output)
			}
			if strings.Contains(string(output), "LEASE_ADMITTED") {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if admitted != 1 {
		t.Fatalf("admitted=%d, want exactly 1", admitted)
	}
	if len(readTestLedger(t, path)["attempts"].([]any)) != 1 {
		t.Fatal("rejected lease charged a call")
	}
}
func TestCrossLanguageLeaseAndLaunchFence(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	account := fixtureAccount
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, TaskID: "fence", MaxCalls: 10, AccountRef: &account})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.BeginLaunch(); err != nil {
		t.Fatal(err)
	}
	if output, err := pythonCommand(t, `from makewand import call_budget as b
try:b.reserve('codex','fast',account_ref='codex:account:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')
except b.BudgetError:print('refused')
else:raise RuntimeError('lease stolen')`).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	if err := a.LaunchFailed(); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Failed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if len(readTestLedger(t, path)["leases"].(map[string]any)) != 0 {
		t.Fatal("settled lease retained")
	}
}
func TestLeaseMalformedProcessFailsClosed(t *testing.T) {
	path := layeredFixture(t)
	data := map[string]any{"schema": 1, "maximum": 10, "attempts": []any{}, "leases": map[string]any{"bad": map[string]any{"owner": nil}}}
	body, _ := json.Marshal(data)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "bad"})
	if _, err := Reserve(ctx, Metadata{Engine: "fake"}); err == nil {
		t.Fatal("malformed lease admitted")
	}
}
func TestLiveWorkerOutlastsDeadLeaseOwner(t *testing.T) {
	if leasePlatformUnsupported() {
		t.Skip("Linux process identity required")
	}
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	// A fixed Python sleep is the only worker; no provider executable is invoked.
	worker := pythonCommand(t, "import time;time.sleep(20)")
	setLeaseFixtureGroup(worker)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Process.Kill(); _ = worker.Wait() }()
	account := fixtureAccount
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 10, TaskID: "worker", AccountRef: &account})
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.BeginLaunch(); err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterWorker(worker.Process.Pid); err != nil {
		t.Fatal(err)
	}
	err = withLockedFile(t.Context(), path, nil, func(p *accountingPath) error {
		data, err := readLedger(p, 10)
		if err != nil {
			return err
		}
		l := data["leases"].(map[string]any)[a.ID].(map[string]any)
		l["expires_at"] = 0
		l["owner"].(map[string]any)["boot"] = "previous-boot"
		return saveLedger(p, data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatal(err)
	}
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = worker.Wait()
	if _, err := Reserve(ctx, Metadata{Engine: "codex"}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseHeartbeatAutomaticallyExtendsExpiry(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "heartbeat", MaxCalls: 10})
	a, err := Reserve(ctx, Metadata{Engine: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Complete(Outcome{Status: Passed, Known: true}) }()
	initial := readTestLedger(t, path)["leases"].(map[string]any)[a.ID].(map[string]any)["heartbeat_at"].(float64)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		beat := readTestLedger(t, path)["leases"].(map[string]any)[a.ID].(map[string]any)["heartbeat_at"].(float64)
		if beat > initial {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("automatic heartbeat did not refresh the persisted lease")
}

func TestUnknownAndKnownAccountsCannotAssertIndependentPools(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	ref := fixtureAccount
	known := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "known", MaxCalls: 10, AccountRef: &ref})
	unknown := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "unknown", MaxCalls: 10})
	a, err := Reserve(known, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(unknown, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	a, err = Reserve(unknown, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(known, Metadata{Engine: "codex"}); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
}

func TestAgyLeaseAliasesCompeteWithPython(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "100")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	cmd := pythonCommand(t, `from makewand import call_budget as b
b.reserve('agy', 'fast', task_id='python-agy')`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python: %v %s", err, out)
	}
	for _, engine := range []string{"gemini", "antigravity", "agy"} {
		ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "go-agy", MaxCalls: 100})
		if a, err := Reserve(ctx, Metadata{Engine: engine}); !errors.Is(err, ErrConcurrencyBusy) {
			if a != nil {
				_ = a.Complete(Outcome{Status: Unknown})
			}
			t.Errorf("%s bypassed Python agy lease: %v", engine, err)
		}
	}
}

func TestAgyAPILeaseAliasesAcrossLanguages(t *testing.T) {
	for _, first := range []string{"python", "go"} {
		t.Run(first, func(t *testing.T) {
			path := layeredFixture(t)
			t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
			t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "100")
			t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
			ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "go-api", MaxCalls: 100})
			if first == "python" {
				cmd := pythonCommand(t, `from makewand import call_budget as b
b.reserve('agy', 'fast', task_id='python-api', account_ref='agy:api:fixture-key')`)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("python: %v %s", err, out)
				}
			} else {
				account := "gemini:api:fixture-key"
				a, err := Reserve(ContextWithConfig(ctx, Config{AccountRef: &account}), Metadata{Engine: "gemini"})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Complete(Outcome{Status: Unknown})
				cmd := pythonCommand(t, `from makewand import call_budget as b
for engine in ('agy', 'gemini', 'antigravity'):
 try: b.reserve(engine, 'fast', task_id='python-api', account_ref=engine+':api:fixture-key')
 except b.BudgetError as e:
  assert 'concurrency' in str(e)
 else: raise RuntimeError('API alias bypassed Go lease')`)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("python: %v %s", err, out)
				}
			}
			for _, engine := range []string{"agy", "gemini", "antigravity"} {
				account := engine + ":api:fixture-key"
				if a, err := Reserve(ContextWithConfig(ctx, Config{AccountRef: &account}), Metadata{Engine: engine}); !errors.Is(err, ErrConcurrencyBusy) {
					if a != nil {
						_ = a.Complete(Outcome{Status: Unknown})
					}
					t.Fatalf("%s bypassed API capacity: %v", engine, err)
				}
			}
			a, err := Reserve(ctx, Metadata{Engine: "agy"})
			if err != nil {
				t.Fatalf("API lease occupied subscription pool: %v", err)
			}
			_ = a.Complete(Outcome{Status: Unknown})
		})
	}
}

func TestAgyAPILeaseKeyNormalization(t *testing.T) {
	want := []any{"agy:api:provider", "agy:api:unknown", "agy:api:unknown:subscription"}
	for _, engine := range []string{"agy", "gemini", "antigravity", "agy:api", "gemini:api", "antigravity:api"} {
		for _, source := range []string{"agy", "gemini", "antigravity"} {
			account := source + ":api:fixture-key"
			if got := leaseKeys(engine, &account); !reflect.DeepEqual(got, want) {
				t.Errorf("%s/%s: got %v, want %v", engine, source, got, want)
			}
		}
	}
}
