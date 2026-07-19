package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

// TestBuildSystemPromptUntrustedOmitsProjectRules verifies that when the Router
// is in untrusted-repo mode, buildSystemPrompt does not inject the repo-provided
// .makewand/rules.md content as trusted "Project rules". Trusted mode (the
// default) must keep the existing behavior of including it.
func TestBuildSystemPromptUntrustedOmitsProjectRules(t *testing.T) {
	dir := t.TempDir()

	rulesDir := filepath.Join(dir, ".makewand")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.makewand): %v", err)
	}
	const marker = "REPO_RULE_MARKER_UNTRUSTED"
	if err := os.WriteFile(filepath.Join(rulesDir, "rules.md"), []byte(marker), 0o600); err != nil {
		t.Fatalf("WriteFile(rules.md): %v", err)
	}
	// A source file keeps the project non-empty so repo context is packaged.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(main.go): %v", err)
	}

	proj, err := engine.OpenProject(dir)
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}

	t.Run("trusted includes project rules", func(t *testing.T) {
		r := mustNewRouterFromConfig(t, model.RouterConfig{})
		prompt := BuildSystemPrompt(proj, model.TaskCode, model.ModeBalanced, r)
		if !strings.Contains(prompt, "Project rules:") || !strings.Contains(prompt, marker) {
			t.Fatalf("trusted prompt should include project rules and marker; got:\n%s", prompt)
		}
	})

	t.Run("untrusted omits project rules", func(t *testing.T) {
		r := mustNewRouterFromConfig(t, model.RouterConfig{})
		r.SetRepoTrust(model.RepoTrustUntrusted)
		prompt := BuildSystemPrompt(proj, model.TaskCode, model.ModeBalanced, r)
		if strings.Contains(prompt, "Project rules:") {
			t.Fatalf("untrusted prompt must not include a Project rules section; got:\n%s", prompt)
		}
		if strings.Contains(prompt, marker) {
			t.Fatalf("untrusted prompt must not include repo rules content; got:\n%s", prompt)
		}
	})
}
