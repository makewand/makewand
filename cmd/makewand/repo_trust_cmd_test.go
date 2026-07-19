package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

// TestPersistentPreRunRejectsInvalidRepoTrust verifies that an invalid
// --repo-trust value is rejected once, globally, by the root command's
// PersistentPreRunE — before any subcommand's RunE and before any backend
// check. This must hold on serve/doctor/setup (which never resolved the flag
// themselves) as well as on the root command.
func TestPersistentPreRunRejectsInvalidRepoTrust(t *testing.T) {
	// Isolate config so a stray real config cannot influence the run.
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())

	const wantMsg = `invalid --repo-trust "bogus": must be "trusted" or "untrusted"`

	cases := []struct {
		name string
		args []string
	}{
		{name: "root", args: []string{"--repo-trust=bogus"}},
		{name: "serve", args: []string{"serve", "--repo-trust=bogus"}},
		{name: "doctor", args: []string{"doctor", "--repo-trust=bogus"}},
		{name: "setup", args: []string{"setup", "--repo-trust=bogus"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tc.args)
			// Silence output so the test log stays clean.
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("Execute(%v) error = nil, want rejection", tc.args)
			}
			if !strings.Contains(err.Error(), wantMsg) {
				t.Fatalf("Execute(%v) error = %q, want it to contain %q", tc.args, err.Error(), wantMsg)
			}
		})
	}
}

// TestPersistentPreRunAcceptsValidRepoTrust verifies that a valid --repo-trust
// value passes validation and lands in the shared resolved value for reuse.
func TestPersistentPreRunAcceptsValidRepoTrust(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	// A configured (remote) backend keeps doctor's model-configuration check
	// green so Execute exercises the accept path cleanly. No network call is made
	// without --remote-check.
	t.Setenv("MAKEWAND_REMOTE_URL", "http://127.0.0.1:8080")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "secret")

	// doctor is a subcommand whose RunE is safe to run (no port bind); it reads
	// resolvedRepoTrust set by PersistentPreRunE.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo-trust=untrusted", "--modes", "balanced"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(doctor --repo-trust=untrusted) error = %v", err)
	}
	if resolvedRepoTrust != model.RepoTrustUntrusted {
		t.Fatalf("resolvedRepoTrust = %v, want %v", resolvedRepoTrust, model.RepoTrustUntrusted)
	}
}

// TestProbeProviderUntrustedSafe verifies the doctor probe's fail-closed guard:
// a direct API provider is safe, a local CLI provider is not.
func TestProbeProviderUntrustedSafe(t *testing.T) {
	if got := probeProviderUntrustedSafe(model.NewClaude("test-key", "")); !got {
		t.Fatalf("probeProviderUntrustedSafe(Claude API) = false, want true")
	}
	if got := probeProviderUntrustedSafe(model.NewClaudeCLI("/usr/bin/claude")); got {
		t.Fatalf("probeProviderUntrustedSafe(Claude CLI) = true, want false")
	}
}

// TestDoctorProbeUntrustedSkipsUnsafeProvider verifies that
// `doctor --probe --repo-trust=untrusted` never executes a provider that is not
// untrusted-repo-safe: the unsafe CLI fixture's sentinel is never written, and
// the probe reports the untrusted-mode skip instead of running Chat.
func TestDoctorProbeUntrustedSkipsUnsafeProvider(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	// Keep quota refresh from touching the developer's real home state.
	t.Setenv("HOME", cfgDir)

	sentinel := filepath.Join(cfgDir, "unsafe_provider_ran")
	script := filepath.Join(cfgDir, "unsafe_provider.sh")
	body := "#!/bin/sh\ntouch \"" + sentinel + "\"\nprintf 'service healthy.\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture script must be executable.
		t.Fatalf("WriteFile(script): %v", err)
	}

	cfg := config.DefaultConfig()
	// Only an unsafe subscription CLI provider is available.
	cfg.CustomProviders = []config.CustomProvider{
		{Name: "claude", Command: script, Access: "subscription", PromptMode: config.CustomPromptModeStdin},
	}

	report, _, _ := runDoctor(cfg, nil, doctorOptions{
		modes:        []model.UsageMode{model.ModeBalanced},
		probe:        true,
		probeTimeout: 5 * time.Second,
		probeRetries: 0,
		repoTrust:    model.RepoTrustUntrusted,
	})

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("unsafe provider was executed in untrusted probe (sentinel %q exists)", sentinel)
	}

	if len(report.LiveProbe) == 0 {
		t.Fatalf("expected at least one live probe report")
	}
	foundSkip := false
	for _, p := range report.LiveProbe {
		if strings.Contains(strings.ToLower(p.Error), "untrusted mode") {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Fatalf("live probe did not report an untrusted-mode skip: %+v", report.LiveProbe)
	}
}
