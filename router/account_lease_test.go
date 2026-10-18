package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

func TestLayeredBudgetAndAccountRefusalNeverStartCLI(t *testing.T) {
	for _, scope := range []string{"machine", "account"} {
		t.Run(scope, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("MAKEWAND_CALL_BUDGET_FILE", filepath.Join(root, "calls.json"))
			t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
			t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "10")
			t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_ROOT", root)
			t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_TASK_ID", "")
			const provider = "lease-fixture"
			seed := execution.ContextWithConfig(context.Background(), execution.Config{TaskID: "seed"})
			a, err := execution.Reserve(seed, execution.Metadata{Engine: provider})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = a.Complete(execution.Outcome{Status: execution.Passed, Known: true}) }()
			if scope == "machine" {
				t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "1")
				if err := a.Complete(execution.Outcome{Status: execution.Passed, Known: true}); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(root, "provider-started")
			command := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "counter", StateFile: marker, Stdout: "fixture"})
			p := NewCommandCLI(provider, command, nil, "legacy")
			ctx := execution.ContextWithConfig(context.Background(), execution.Config{TaskID: "rejected"})
			_, _, err = ChatProvider(ctx, p, []Message{{Role: "user", Content: "offline"}}, "", 16)
			want := execution.ErrBudgetExhausted
			if scope == "account" {
				want = execution.ErrConcurrencyBusy
			}
			if !errors.Is(err, want) {
				t.Fatalf("refusal: %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("provider started despite %s refusal: %v", scope, err)
			}
		})
	}
}
