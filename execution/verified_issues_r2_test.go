package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestVerifiedAPILeaseAliases(t *testing.T) {
	for canonical, aliases := range map[string][]string{
		"codex": {"openai", "CODEX"}, "claude": {"anthropic", "CLAUDE"},
		"agy": {"google", "gemini", "antigravity"}, "grok": {"xai"},
		"local": {"ollama"}, "qwen": {"dashscope", "aliyun"},
	} {
		account := canonical + ":api:fixture"
		want := leaseKeys(canonical, &account)
		for _, alias := range aliases {
			account = alias + ":api:fixture"
			if got := leaseKeys(alias, &account); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: got %v, want %v", alias, got, want)
			}
		}
	}
}

func TestVerifiedCrossLanguageAliasGolden(t *testing.T) {
	out, err := pythonCommand(t, `import json
from makewand.config import PROVIDER_ALIASES
from makewand.account_leases import keys
names = set(PROVIDER_ALIASES) | set(PROVIDER_ALIASES.values()) | {'antigravity'}
names |= {name+'-api' for name in names}
names |= {name.upper() for name in names}
names |= {name+':api' for name in names}
print(json.dumps({name: keys(name, name+':api:fixture') for name in names}))`).Output()
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string][]any
	if err := json.Unmarshal(out, &golden); err != nil {
		t.Fatal(err)
	}
	for name, want := range golden {
		account := name + ":api:fixture"
		if got := leaseKeys(name, &account); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Go %v != Python %v", name, got, want)
		}
	}
}

func TestVerifiedPythonCodexAPIBlocksGoOpenAI(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "10")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	cmd := pythonCommand(t, `from makewand import call_budget as b
b.reserve('codex','fast',task_id='python',account_ref='codex:api:fixture')`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	account := "openai:api:fixture"
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "go", MaxCalls: 10, AccountRef: &account})
	if _, err := Reserve(ctx, Metadata{Engine: "openai"}); err == nil || errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("expected capacity refusal, got %v", err)
	}
	if len(readTestLedger(t, path)["attempts"].([]any)) != 1 {
		t.Fatal("API alias admission was charged")
	}
}

func TestVerifiedDeadOwnerLaunchFenceReclaimed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux boot identity fixture")
	}
	owner := leaseProcessRef(os.Getpid())
	owner["boot"] = "previous-boot"
	data := map[string]any{"leases": map[string]any{"old": map[string]any{
		"keys": leaseKeys("codex", nil), "owner": owner, "workers": []any{},
		"expires_at": json.Number("0"), "launch_pending": true,
	}}}
	if _, err := admitLease(data, "codex", nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := data["leases"].(map[string]any)["old"]; exists {
		t.Fatal("dead launch fence retained")
	}
}

func TestVerifiedCompletedLeaseReclaimedWithLiveOwner(t *testing.T) {
	data := map[string]any{
		"attempts": []any{map[string]any{"id": "old", "status": "completed"}},
		"leases": map[string]any{"old": map[string]any{
			"keys": leaseKeys("codex", nil), "owner": leaseProcessRef(os.Getpid()), "workers": []any{},
			"expires_at": json.Number("0"), "launch_pending": false,
		}},
	}
	if _, err := admitLease(data, "codex", nil); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedCapacityWaitAndRetryableRefusal(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0.2")
	ctx := ContextWithConfig(context.Background(), Config{LedgerPath: path, TaskID: "capacity", MaxCalls: 10})
	first, err := Reserve(ctx, Metadata{Engine: "agy"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Complete(Outcome{Status: Passed, Known: true})
	started := time.Now()
	second, err := Reserve(ctx, Metadata{Engine: "agy"})
	if second != nil || err == nil || errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("contention is not exhausted budget: %v", err)
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("capacity refusal did not wait")
	}
	if len(readTestLedger(t, path)["attempts"].([]any)) != 1 {
		t.Fatal("busy admission spent a call")
	}
	done := make(chan error, 1)
	go func() {
		a, err := Reserve(ctx, Metadata{Engine: "agy"})
		if err == nil {
			err = a.Complete(Outcome{Status: Passed, Known: true})
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := first.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
