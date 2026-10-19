package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
)

// Corrupt the ledger only after Reserve persisted admission, before the
// router's final cancellation check / Start boundary. No provider can run.
type r5AdmissionFaultContext struct {
	context.Context
	path   string
	cancel context.CancelFunc
	once   sync.Once
	raw    []byte
	err    error
}

func (c *r5AdmissionFaultContext) Err() error {
	raw, err := os.ReadFile(c.path)
	if err == nil && strings.Contains(string(raw), `"status": "reserved"`) {
		c.once.Do(func() {
			c.raw = raw
			c.err = os.WriteFile(c.path, []byte("{"), 0600)
			if c.cancel != nil {
				c.cancel()
			}
		})
	}
	return c.Context.Err()
}

func TestR5RouterStartAndLateCancellationReleaseAfterTransientFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "start-fails", true: "release-fails"}[canceled], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
			t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_DAILY_CALLS", "100")
			t.Setenv("MAKEWAND_PROJECT_ROOT", t.TempDir())
			t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
			t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
			base, path := budgetTestContext(t, 1)
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			fault := &r5AdmissionFaultContext{Context: ctx, path: path}
			if canceled {
				fault.cancel = cancel
			}
			var calls atomic.Int32
			p := &dispatchBudgetProvider{name: "grok", calls: &calls}
			_, _, err := reserveProvider(fault, p)
			if fault.err != nil || len(fault.raw) == 0 {
				t.Fatalf("fault not exercised: %v", fault.err)
			}
			if restoreErr := os.WriteFile(path, fault.raw, 0600); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if err == nil || !errors.Is(err, errExecutionAccounting) {
				t.Fatalf("startup / cleanup failure lost: %v", err)
			}
			if canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("late admission cancellation lost: %v", err)
			}
			until := time.Now().Add(3 * time.Second)
			for budgetTestAttempts(t, path)[0]["status"] != "released" {
				if time.Now().After(until) {
					t.Fatal("failed startup left reservation reserved")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if calls.Load() != 0 {
				t.Fatal("startup failure launched provider")
			}
			if _, _, err := ChatProvider(base, p, nil, "", 16); err != nil {
				t.Fatalf("failed startup did not refund account / scope capacity: %v", err)
			}
		})
	}
}

func TestR5DirectCodexPreservesConfigurationAndAccountBinding(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			home, work := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CODEX_HOME", "")
			root := filepath.Join(home, ".codex")
			if explicit {
				root = t.TempDir()
				t.Setenv("CODEX_HOME", root)
			}
			files := []string{"config.toml", "AGENTS.md", "AGENTS.override.md", "skills/guard/SKILL.md", "rules/default.rules", "hooks/guard.sh", "hooks.json"}
			for _, name := range files {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("user configuration"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			auth := filepath.Join(root, "auth.json")
			if err := os.WriteFile(auth, []byte(`{"tokens":{"account_id":"before"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, err := bindCredentialAdmission(t.Context(), "codex")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("fixture")
			cmd.Dir = work
			cmd.Env = []string{"HOME=" + home}
			if explicit {
				cmd.Env = append(cmd.Env, "CODEX_HOME="+root)
			}
			if err := freezeDirectCLIAccount(ctx, "codex", cmd); err != nil {
				t.Fatal(err)
			}
			defer cliAfterWait(cmd)
			effective := cliEnvValue(cmd.Env, "CODEX_HOME")
			if effective == "" {
				effective = root
			}
			for _, name := range files {
				if raw, err := os.ReadFile(filepath.Join(effective, filepath.FromSlash(name))); err != nil || string(raw) != "user configuration" {
					t.Fatalf("missing direct Codex configuration %s: %v", name, err)
				}
			}
			if effective != root {
				t.Fatalf("Codex configuration path changed to %s", effective)
			}
			if err := os.WriteFile(auth, []byte(`{"tokens":{"account_id":"after"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyDirectCLIAccount(cmd); !errors.Is(err, execution.ErrUnknownOutcome) {
				t.Fatalf("login switch did not invalidate attribution: %v", err)
			}
			if err := freezeDirectCLIAccount(ctx, "codex", cmd); err == nil {
				t.Fatal("post-admission account switch accepted")
			}
		})
	}
}

func TestR5DirectCodexCredentialRotationNeedsKnownAccount(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "known-refresh"}[known], func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CODEX_HOME", root)
			account := ""
			if known {
				account = `"account_id":"same",`
			}
			auth := filepath.Join(root, "auth.json")
			if err := os.WriteFile(auth, []byte(`{"tokens":{`+account+`"access_token":"fixture-before"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, err := bindCredentialAdmission(t.Context(), "codex")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("fixture")
			cmd.Dir = t.TempDir()
			cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + root}
			if err := freezeDirectCLIAccount(ctx, "codex", cmd); err != nil {
				t.Fatal(err)
			}
			defer cliAfterWait(cmd)
			if err := os.WriteFile(auth, []byte(`{"tokens":{`+account+`"access_token":"fixture-after"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			err = verifyDirectCLIAccount(cmd)
			if known && err != nil || !known && !errors.Is(err, execution.ErrUnknownOutcome) {
				t.Fatalf("credential rotation attribution: known=%v error=%v", known, err)
			}
		})
	}
}

func TestR5AdmissionCancellationDoesNotPoisonHealth(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			t.Setenv("MAKEWAND_LAYERED_BUDGET", "0")
			t.Setenv("MAKEWAND_CALL_BUDGET_FILE", "")
			t.Setenv("MAKEWAND_MAX_MODEL_CALLS", "")
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
			ctx, _ := budgetTestContext(t, 10)
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			defer cancel()
			var calls atomic.Int32
			p := &dispatchBudgetProvider{name: "grok", calls: &calls}
			_, _, err := reserveProvider(ctx, p)
			if err == nil || errors.Is(err, errExecutionAccounting) || stopExecutionReplay(err) {
				t.Fatalf("admission cancellation must allow fallback: %v", err)
			}
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"grok": {Provider: p, Access: AccessSubscription}}})
			ac := &attemptContext{ctx: t.Context()}
			id := attemptIdentity{name: "grok", provider: p}
			// The caller is live, but its shorter attempt deadline expired in
			// admission. Test the health handoff independently of a real CLI.
			for range 3 {
				r.recordStreamAttemptError(ac, id, time.Now(), err)
			}
			if open, _ := r.isCircuitOpen("grok"); open {
				t.Fatal("admission cancellation opened provider circuit")
			}
			if r.usage.FailureCount("grok") != 0 {
				t.Fatal("admission cancellation penalized reliability")
			}
			if calls.Load() != 0 {
				t.Fatal("admission failure launched provider")
			}
			// A real transport timeout must retain the provider penalty.
			r.recordStreamAttemptError(ac, id, time.Now(), context.DeadlineExceeded)
			if open, _ := r.isCircuitOpen("grok"); !open {
				t.Fatal("provider timeout stopped affecting health")
			}
		})
	}
}

func TestR5StaleForeignAccountDirectoryDoesNotBlockGrok(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap arguments are Linux specific")
	}
	old := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = old })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		for _, state := range []string{"missing", "not-directory", "denied"} {
			t.Run(variable+"/"+state, func(t *testing.T) {
				home, work, external := t.TempDir(), t.TempDir(), t.TempDir()
				t.Setenv("HOME", home)
				path := filepath.Join(external, "stale", "account")
				if state == "not-directory" {
					if err := os.WriteFile(filepath.Dir(path), []byte("file"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if state == "denied" {
					if os.Geteuid() == 0 {
						t.Skip("root bypasses directory search permission")
					}
					if err := os.MkdirAll(path, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(filepath.Dir(path), 0000); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
							t.Fatal(err)
						}
					}()
				}
				cmd := exec.Command("fixture")
				cmd.Dir = work
				cmd.Env = []string{"HOME=" + home, variable + "=" + path}
				wrapped, err := wrapCLICommandWithSandbox(t.Context(), "grok", cmd)
				if err != nil {
					t.Fatalf("foreign stale account blocked unrelated provider: %v", err)
				}
				defer cliAfterWait(wrapped)
				for _, arg := range wrapped.Args {
					if arg == path {
						t.Fatal("stale directory added to mounts")
					}
				}
			})
		}
	}
}

func TestR5AccountDirectoryErrorsNameSelectedVariable(t *testing.T) {
	home := t.TempDir()
	_, _, err := cliConfiguredAccountHome("claude", "CLAUDE_CONFIG_DIR", []string{"CLAUDE_CONFIG_DIR=" + home}, t.TempDir(), home)
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") || strings.Contains(err.Error(), "CODEX_HOME") {
		t.Fatalf("wrong account environment variable in error: %v", err)
	}
}
