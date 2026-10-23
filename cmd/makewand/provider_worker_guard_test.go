package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/testfixture"
	"github.com/spf13/cobra"
)

// TestMain drops a parent provider-worker marker. The guard itself is covered
// by TestRefuseProviderWorkerDispatch; leaving MAKEWAND_PROVIDER_WORKER=1 set
// makes every in-process command refuse to start. Fixture dispatches record
// provider evidence, which stays out of the user cache.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("MAKEWAND_PROVIDER_WORKER")
	cleanupEvidence, err := testfixture.IsolateExecutionEvidence()
	if err != nil {
		fmt.Fprintln(os.Stderr, "execution evidence isolation:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanupEvidence()
	os.Exit(code)
}

func TestProviderWorkerSharedPolicy(t *testing.T) {
	t.Setenv("MAKEWAND_PROVIDER_WORKER", "1")
	data, err := os.ReadFile("../../tests/testdata/provider_worker_policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Argv    []string `json:"argv"`
		Allowed bool     `json:"allowed"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		if err := refuseProviderWorkerArgs(test.Argv); (err == nil) != test.Allowed {
			t.Errorf("%q: allowed=%v, error=%v", test.Argv, test.Allowed, err)
		}
	}
	t.Setenv("MAKEWAND_PROVIDER_WORKER", "")
	if err := refuseProviderWorkerArgs([]string{"state", "restore"}); err != nil {
		t.Fatal(err)
	}
}

func TestRefuseProviderWorkerDispatch(t *testing.T) {
	root := newRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	find := func(args ...string) *cobra.Command {
		t.Helper()
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("find %v: %v", args, err)
		}
		return cmd
	}
	t.Setenv("MAKEWAND_PROVIDER_WORKER", "")
	if err := refuseProviderWorkerDispatch(find("chat")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_PROVIDER_WORKER", "1")
	for _, group := range []string{"state", "token", "user", "audit", "usage"} {
		cmd := find(group)
		for _, child := range append([]*cobra.Command{cmd}, cmd.Commands()...) {
			if err := refuseProviderWorkerDispatch(child); err == nil {
				t.Errorf("allowed %s", child.CommandPath())
			}
		}
	}
	for _, path := range [][]string{{}, {"chat"}, {"new"}, {"serve"}, {"setup"}, {"doctor"}, {"quota"}} {
		if err := refuseProviderWorkerDispatch(find(path...)); err == nil {
			t.Errorf("allowed %v", path)
		}
	}
	for _, path := range [][]string{{"help"}, {"completion", "bash"}, {"completion", "fish"}, {"completion", "powershell"}, {"completion", "zsh"}} {
		if err := refuseProviderWorkerDispatch(find(path...)); err != nil {
			t.Errorf("refused %v: %v", path, err)
		}
	}
	safe := find("completion", "fish")
	// New children and provider flags must not inherit an ancestor's permission.
	child := &cobra.Command{Use: "future"}
	safe.AddCommand(child)
	if err := refuseProviderWorkerDispatch(child); err == nil {
		t.Fatal("new child allowed")
	}
	for _, flag := range []string{"probe", "provider-start", "future-flag"} {
		safe.Flags().Bool(flag, false, "fixture")
		if err := safe.Flags().Set(flag, "false"); err != nil {
			t.Fatal(err)
		}
		if err := refuseProviderWorkerDispatch(safe); err == nil {
			t.Fatalf("new flag %s allowed", flag)
		}
	}
	// A Cobra alias or enabled prefix must not turn a refused command into a query.
	management := find("state", "backup")
	management.Aliases = []string{"query"}
	if err := refuseProviderWorkerDispatch(find("state", "query")); err == nil {
		t.Fatal("management alias allowed")
	}
	root.PersistentFlags().Bool("future-provider", false, "fixture")
	if err := root.PersistentFlags().Set("future-provider", "false"); err != nil {
		t.Fatal(err)
	}
	if err := refuseProviderWorkerDispatch(find("help")); err == nil {
		t.Fatal("inherited future flag allowed")
	}
}

func TestProviderWorkerBinaryNoSideEffects(t *testing.T) {
	if testing.Short() {
		t.Skip("binary fixture")
	}
	bin := buildMakewandBinary(t)
	for _, args := range [][]string{{"state", "backup", "backup.tar.gz"}, {"token", "issue", "--json"}, {"user", "role", "fixture", "admin"}, {"--cwd=help", "state", "restore", "fake.tar.gz", "--force"}, {"completion", "bash"}, {"help"}, {"--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(bin, args...)
			cmd.Dir = dir
			cmd.Env = append(backendFixtureEnv(dir), "MAKEWAND_PROVIDER_WORKER=1", "MAKEWAND_MAX_MODEL_CALLS=1", "TMPDIR="+dir, "TMP="+dir, "TEMP="+dir)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			err := cmd.Run()
			if args[0] == "completion" || args[0] == "help" || args[0] == "--help" {
				if err != nil {
					t.Fatalf("query failed: %v: %s", err, &output)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(output.String(), "只读") {
					t.Fatalf("expected worker refusal: %v: %s", err, &output)
				}
			}
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("worker created files in %s: %v", filepath.Base(dir), files)
			}
		})
	}
}
