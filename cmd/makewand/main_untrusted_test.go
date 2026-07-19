package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

// TestRunSinglePromptUntrustedSurfacesActionableMessage verifies the headless
// candidate-selection path propagates the engine's fail-closed sentinel: in
// untrusted-repo mode with no untrusted-repo-safe provider, the engine returns a
// CandidateSelection carrying ErrNoUntrustedSafeProvider, and runSinglePrompt
// must surface the actionable untrusted-mode message (RepoTrustNoSafeProvider),
// not the generic "no candidate provider produced a response".
func TestRunSinglePromptUntrustedSurfacesActionableMessage(t *testing.T) {
	// Hermetic environment: no real config/creds, no CLI detection surprises.
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	// Autopilot + a fix task drives the headless candidate-selection branch.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/untrusted\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(go.mod): %v", err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(dir): %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeAuto
	// No API keys and no CLIs: in untrusted mode the candidate provider set is
	// empty, which is exactly the fail-closed case the engine flags.

	err = runSinglePrompt(cfg, "/fix the broken build", 5*time.Second, model.RepoTrustUntrusted, false)
	if err == nil {
		t.Fatalf("runSinglePrompt returned nil, want fail-closed untrusted-mode error")
	}

	want := i18n.Msg().RepoTrustNoSafeProvider
	if err.Error() != want {
		t.Fatalf("runSinglePrompt error = %q, want the actionable untrusted-mode message %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "no candidate provider produced a response") {
		t.Fatalf("runSinglePrompt returned the generic error, want the actionable untrusted-mode message")
	}
}
