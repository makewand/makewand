package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfiguredCodexHomeUsesChildEnvironmentAndDedicatedDirectory(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "work")
	selected := filepath.Join(root, "account")
	foreignAccount := filepath.Join(home, ".claude")
	workspaceState := filepath.Join(workspace, "state")
	for _, path := range []string{home, workspace, selected, foreignAccount, workspaceState, filepath.Join(home, ".ssh")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", filepath.Join(root, "parent-account-must-not-be-used"))
	if got, _, err := cliConfiguredCodexHome([]string{}, workspace, home); got != "" || err != nil {
		t.Fatalf("empty child environment inherited account: %q %v", got, err)
	}
	got, real, err := cliConfiguredCodexHome([]string{"CODEX_HOME=missing", "CODEX_HOME=../account"}, workspace, home)
	if err != nil || got != selected || real != selected {
		t.Fatalf("child account resolution: %q %q %v", got, real, err)
	}
	for _, path := range []string{root, home, workspace, workspaceState, foreignAccount, filepath.Join(home, ".ssh"), filepath.Join(root, "missing")} {
		if _, _, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + path}, workspace, home); err == nil {
			t.Errorf("unsafe/unavailable account directory accepted: %q", path)
		}
	}
}

func TestConfiguredCodexHomeRejectsWorkspaceSymlinkToExternalAccount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs Windows privileges")
	}
	root := t.TempDir()
	workspace, account := filepath.Join(root, "work"), filepath.Join(root, "account")
	for _, path := range []string{workspace, account} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	selected := filepath.Join(workspace, "account-link")
	if err := os.Symlink(account, selected); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + selected}, workspace, filepath.Join(root, "home")); err == nil {
		t.Fatal("account mounted inside workspace through a symlink")
	}
}

func TestWrapCodexHomeUsesSelectedAccountWithoutDefaultAccount(t *testing.T) {
	oldLookup := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = oldLookup })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	selected, defaultAccount := filepath.Join(home, ".codex-other"), filepath.Join(home, ".codex")
	for _, path := range []string{workspace, selected, defaultAccount} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = workspace
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + selected}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "codex", cmd)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(wrapped.Args, " ")
	if !strings.Contains(args, "--bind "+selected+" "+selected) || !strings.Contains(args, "--setenv CODEX_HOME "+selected) {
		t.Fatalf("selected account missing: %v", wrapped.Args)
	}
	if strings.Contains(args, "--bind "+defaultAccount+" "+defaultAccount) || !strings.Contains(args, "--tmpfs "+defaultAccount) {
		t.Fatalf("default account was exposed: %v", wrapped.Args)
	}
	foreign, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	foreignArgs := strings.Join(foreign.Args, " ")
	if strings.Contains(foreignArgs, "--bind "+selected+" "+selected) || !strings.Contains(foreignArgs, "--tmpfs "+selected) || cliEnvValue(foreign.Env, "CODEX_HOME") != "" {
		t.Fatalf("foreign provider can access selected account: %v", foreign.Args)
	}
}

func TestWrapCodexHomeRealSandboxSelectedSymlinkAndForeignIsolation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native bubblewrap regression requires Linux")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap unavailable")
	}
	if probe := exec.Command(bwrap, "--ro-bind", "/", "/", "--", "/usr/bin/true"); probe.Run() != nil {
		if os.Getenv("MAKEWAND_TEST_REQUIRE_BWRAP") == "1" {
			t.Fatal("required bubblewrap probe failed")
		}
		t.Skip("bubblewrap unavailable in this environment")
	}
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	account, other := filepath.Join(root, "account"), filepath.Join(home, ".codex")
	for _, path := range []string{workspace, account, other} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(account, "marker"), []byte("SELECTED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "marker"), []byte("OTHER_ACCOUNT"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".codex-selected")
	if err := os.Symlink(account, link); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"codex", "claude"} {
		script := `if test -r "$CODEX_HOME/marker"; then cat "$CODEX_HOME/marker"; fi; if test -r "$HOME/.codex/marker"; then echo DEFAULT_LEAK; fi; if test -r "$1"; then echo FOREIGN_LEAK; fi`
		//nolint:gosec // G204: fixed fixture program; only test-owned paths are passed as positional arguments, never shell source.
		cmd := exec.Command("/bin/sh", "-c", script, "fixture", filepath.Join(account, "marker"))
		cmd.Dir = workspace
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "CODEX_HOME=" + link}
		wrapped, err := wrapCLICommandWithSandbox(context.Background(), provider, cmd)
		if err != nil {
			t.Fatal(err)
		}
		out, err := wrapped.CombinedOutput()
		if err != nil {
			t.Fatalf("%s sandbox: %v %s", provider, err, out)
		}
		if strings.Contains(string(out), "DEFAULT_LEAK") || (provider == "codex" && !strings.HasPrefix(string(out), "SELECTED")) || (provider == "claude" && len(out) != 0) {
			t.Fatalf("%s account isolation: %q", provider, out)
		}
	}
}
