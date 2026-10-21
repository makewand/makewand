package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestR6ClaudeBindingIgnoresStateButRejectsLoginChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	root := filepath.Join(home, ".claude")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(root, ".credentials.json")
	state := filepath.Join(home, ".claude.json")
	write := func(path, raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(credentials, `{"fixture":"same"}`)
	write(state, `{"oauthAccount":{"accountUuid":"u-1"},"numStartups":1}`)
	ctx, err := bindCredentialAdmission(context.Background(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"oauthAccount":{"accountUuid":"u-1"},"numStartups":2,"projects":{"/x":{"lastCost":3}}}`
	write(state, raw)
	if err := verifyCredentialBinding(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	binding, _ := ctx.Value(credentialBindingKey{}).(*credentialBinding)
	if err := binding.verify(state, []byte(raw), true); err != nil {
		t.Fatal(err)
	}
	private, err := newCLIPrivateHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer private.cleanup()
	private.binding = binding
	fi, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := private.addProviderEntry(nil, "claude", home, ".claude.json", state, fi, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("fixture")
	cmd.Env = []string{"HOME=" + home}
	if err := freezeDirectCLIAccount(ctx, "claude", cmd); err != nil {
		t.Fatal(err)
	}
	write(state, `{"oauthAccount":{"accountUuid":"u-1"},"numStartups":3}`)
	if err := verifyDirectCLIAccount(cmd); err != nil {
		t.Fatal(err)
	}
	write(state, `{"oauthAccount":{"accountUuid":"u-2"}}`)
	if err := verifyCredentialBinding(ctx, "claude"); err == nil {
		t.Fatal("account change accepted")
	}
	// A known account is bound by its accountUuid: refreshed tokens of the same
	// account are no login change (TestR8ReadableTokenChangeStillRefused keeps
	// the byte comparison for a login without a known account).
	write(state, raw)
	write(credentials, `{"fixture":"changed"}`)
	if err := verifyCredentialBinding(ctx, "claude"); err != nil {
		t.Fatalf("same-account token refresh refused: %v", err)
	}
	if err := os.Remove(credentials); err != nil {
		t.Fatal(err)
	}
	if err := verifyCredentialBinding(ctx, "claude"); err == nil {
		t.Fatal("removed credentials accepted")
	}
}

func TestR6CLIAccountRefusalReleasesUnstartedBudget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap account preparation is Linux specific")
	}
	root := t.TempDir()
	home, work := filepath.Join(root, "home"), filepath.Join(root, "work")
	login := filepath.Join(home, ".claude")
	for _, path := range []string{login, work} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Setenv("MAKEWAND_PROJECT_ROOT", work)
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
	state := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(state, []byte(`{"oauthAccount":{"accountUuid":"before"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	p := NewClaudeCLI("fixture")
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		if err := os.WriteFile(state, []byte(`{"oauthAccount":{"accountUuid":"after"}}`), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("fixture-must-not-start")
		cmd.Dir = work
		cmd.Env = []string{"HOME=" + home}
		return cmd
	}
	old := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = old })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	ctx, path := budgetTestContext(t, 1)
	_, _, err := ChatProvider(ctx, p, nil, "", 10)
	if err == nil {
		t.Fatal("changed account launched")
	}
	entries := budgetTestAttempts(t, path)
	if len(entries) != 1 || entries[0]["status"] != "released" {
		t.Fatalf("account refusal charged: %v", entries)
	}
}
