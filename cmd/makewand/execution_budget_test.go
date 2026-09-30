package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/router"
	"github.com/spf13/cobra"
)

func TestNativeBudgetFlagsInitializePrivateSharedLedger(t *testing.T) {
	for _, name := range []string{"MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_MAX_MODEL_CALLS", "MAKEWAND_TASK_ID"} {
		t.Setenv(name, "")
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--max-model-calls", "2"})
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("MAKEWAND_CALL_BUDGET_FILE")
	// #nosec G703 -- cleanup removes only the native CLI-created private temporary ledger and its own sidecar.
	t.Cleanup(func() { _ = os.Remove(path); _ = os.Remove(path + ".lock") })
	// #nosec G703 -- path is the private temporary ledger created by this test's native CLI flag.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger map[string]any
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger["schema"] != float64(1) || ledger["maximum"] != float64(2) || len(ledger["attempts"].([]any)) != 0 {
		t.Fatalf("ledger incorrectly initialized: %s", data)
	}
	// #nosec G703 -- path is the same private temporary test ledger created above.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("ledger mode=%o", info.Mode().Perm())
	}
	if !execution.ValidIdentifier(os.Getenv("MAKEWAND_TASK_ID")) {
		t.Fatal("task identity not initialized")
	}
}

func TestNativeChildCannotReplaceInheritedBudgetOrRaiseMaximum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent.json")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", path)
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "2")
	t.Setenv("MAKEWAND_TASK_ID", "native-child-test")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--max-model-calls", "10"})
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MAKEWAND_MAX_MODEL_CALLS") != "2" || os.Getenv("MAKEWAND_CALL_BUDGET_FILE") != path {
		t.Fatal("native flags relaxed inherited budget")
	}
	cmd = newRootCmd()
	cmd.SetArgs([]string{"--call-budget-file", filepath.Join(t.TempDir(), "escape.json"), "--max-model-calls", "10"})
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	if err := cmd.Execute(); err == nil {
		t.Fatal("native child switched ledger")
	}
}

func TestNativeBudgetErrorKeepsTypedExitStatus(t *testing.T) {
	t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
	t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--max-model-calls", "0"})
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	err := cmd.Execute()
	if err == nil || execution.ErrorStatus(err) != execution.InvalidRequest || execution.ErrorStatus(err).ExitCode() != 2 {
		t.Fatalf("invalid request exit: %v", err)
	}
	err = &execution.UnknownOutcomeError{Err: errors.New("synthetic lost response")}
	if execution.ErrorStatus(err).ExitCode() != 17 || execution.ErrorStatus(execution.ErrBudgetExhausted).ExitCode() != 13 {
		t.Fatal("native typed errors lost protocol exit status")
	}
}

func TestNativeFailureExitClassification(t *testing.T) {
	known := &router.ProviderError{Provider: "synthetic", Kind: router.ErrorKindRateLimit, StatusCode: 429, Message: "offline quota refusal"}
	for _, test := range []struct {
		name string
		err  error
		exit int
	}{
		{"known provider failure", fmt.Errorf("dispatch: %w", known), 10},
		{"provider configuration", &router.ProviderError{Kind: router.ErrorKindConfig}, 2},
		{"internal error", errors.New("synthetic internal failure"), 1},
		{"unknown provider classification", &router.ProviderError{Kind: router.ErrorKindUnknown}, 1},
		{"unknown outcome wins", &execution.UnknownOutcomeError{Err: known}, 17},
		{"budget wins", &router.ProviderError{Kind: router.ErrorKindProvider, Err: execution.ErrBudgetExhausted}, 13},
		{"deadline wins", &router.ProviderError{Kind: router.ErrorKindProvider, Err: context.DeadlineExceeded}, 16},
		{"cancel wins", &router.ProviderError{Kind: router.ErrorKindProvider, Err: context.Canceled}, 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			if exit := execution.ErrorStatus(test.err).ExitCode(); exit != test.exit {
				t.Fatalf("native exit=%d, want %d: %v", exit, test.exit, test.err)
			}
		})
	}
}
