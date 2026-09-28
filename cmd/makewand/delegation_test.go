package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
)

// go-engine-tui-cmd#5: persistent flags written before a Python subcommand
// used to hide the subcommand, so `makewand --approval safe status` or
// `makewand --repo-trust untrusted run "x"` became a chat prompt sent to a
// model (consuming quota, exit 0).
func TestPlanPythonDelegation_GlobalFlagsBeforeSubcommand(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		subcmd  string // "" = not delegated
		forward []string
		errHas  string
	}{
		{name: "plain", args: []string{"status"}, subcmd: "status", forward: []string{"status"}},
		{name: "repo-trust value", args: []string{"--repo-trust", "untrusted", "run", "hello"}, subcmd: "run", forward: []string{"--repo-trust", "untrusted", "run", "hello"}},
		{name: "repo-trust equals", args: []string{"--repo-trust=untrusted", "status"}, subcmd: "status", forward: []string{"--repo-trust", "untrusted", "status"}},
		{name: "debug", args: []string{"--debug", "models"}, subcmd: "models", forward: []string{"models"}},
		{name: "debug=false", args: []string{"--debug=false", "--repo-trust", "trusted", "probe"}, subcmd: "probe", forward: []string{"--repo-trust", "trusted", "probe"}},
		{name: "approval before subcommand", args: []string{"--approval", "safe", "status"}, errHas: "--approval"},
		{name: "approval equals before run", args: []string{"--approval=manual", "run", "x"}, errHas: "cannot be honored"},
		{name: "invalid approval", args: []string{"--approval", "autopilto", "status"}, errHas: "invalid --approval"},
		{name: "invalid repo-trust", args: []string{"--repo-trust", "bogus", "status"}, errHas: "invalid --repo-trust"},
		{name: "invalid repo-trust after subcommand", args: []string{"run", "--repo-trust=bogus", "x"}, errHas: "invalid --repo-trust"},
		{name: "missing flag value", args: []string{"--repo-trust"}, errHas: "needs an argument"},
		{name: "chat prompt", args: []string{"hello", "world"}},
		{name: "flags then prompt", args: []string{"--approval", "safe", "fix the bug"}},
		{name: "go subcommand", args: []string{"--debug", "setup"}},
		{name: "root-local flag is not skipped", args: []string{"--print", "status"}},
		{name: "mode translation", args: []string{"--repo-trust", "trusted", "run", "--mode", "power", "x"}, subcmd: "run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planPythonDelegation(tc.args)
			if tc.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("err = %v, want error containing %q (delegation=%+v)", err, tc.errHas, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.subcmd == "" {
				if got != nil {
					t.Fatalf("delegated %+v, want Go CLI handling", got)
				}
				return
			}
			if got == nil || got.subcmd != tc.subcmd {
				t.Fatalf("delegation = %+v, want subcommand %q", got, tc.subcmd)
			}
			if tc.forward != nil && strings.Join(got.args, "\x00") != strings.Join(tc.forward, "\x00") {
				t.Fatalf("forwarded args = %q, want %q", got.args, tc.forward)
			}
		})
	}
}

func TestPlanPythonDelegation_TranslatesTierAfterSubcommand(t *testing.T) {
	got, err := planPythonDelegation([]string{"--repo-trust", "trusted", "run", "--mode", "power", "x"})
	if err != nil || got == nil {
		t.Fatalf("delegation = %+v, err = %v", got, err)
	}
	want, ok := engine.ToPythonTier("power")
	if !ok {
		t.Skip("no python tier mapping for power")
	}
	if strings.Join(got.args, " ") != "--repo-trust trusted run --tier "+want+" x" {
		t.Fatalf("args = %q", got.args)
	}
}

func TestPlanPythonDelegation_DebugNote(t *testing.T) {
	got, err := planPythonDelegation([]string{"--debug", "status"})
	if err != nil || got == nil || len(got.notes) == 0 {
		t.Fatalf("expected a note that --debug is ignored: %+v %v", got, err)
	}
}

// TestE2EGlobalFlagsBeforePythonSubcommandNeverReachAModel is the end-to-end
// reproduction of go-engine-tui-cmd#5 with a configured provider fixture: the
// subcommand must be recognized (and delegated, or rejected with a clear
// error), never sent to a model as a chat prompt.
func TestE2EGlobalFlagsBeforePythonSubcommandNeverReachAModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell-based E2E fixtures are Unix-only")
	}
	bin := buildMakewandBinary(t)
	script := writeProviderScript(t)
	cfg := config.DefaultConfig()
	cfg.CustomProviders = []config.CustomProvider{
		{Name: "claude", Command: script, Args: []string{"claude"}, Access: "subscription", PromptMode: config.CustomPromptModeStdin},
	}
	cfgDir := t.TempDir()
	writeTestConfig(t, cfgDir, cfg)

	// PATH is the isolated config dir: no python3, so a correctly delegated
	// subcommand stops with the "python3 runtime is required" error. Before
	// the fix the trusted variant printed the provider fixture's reply
	// ("provider:claude", exit 0): "status" had been sent to a model.
	for _, trust := range []string{"trusted", "untrusted"} {
		stdout, stderr, err := runMakewand(t, bin, cfgDir, "--repo-trust", trust, "status")
		if err == nil || !strings.Contains(stderr, "python3 runtime is required to execute status") {
			t.Fatalf("--repo-trust %s before status was not delegated: err=%v\nstdout:\n%s\nstderr:\n%s", trust, err, stdout, stderr)
		}
		if strings.Contains(stdout, "provider:") {
			t.Fatalf("subcommand was sent to a model as a prompt: %q", stdout)
		}
	}

	stdout, stderr, err := runMakewand(t, bin, cfgDir, "--approval", "safe", "status")
	if err == nil || !strings.Contains(stderr, "--approval") {
		t.Fatalf("--approval before status was not rejected: err=%v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if strings.Contains(stdout, "provider:") {
		t.Fatalf("subcommand was sent to a model as a prompt: %q", stdout)
	}
}
