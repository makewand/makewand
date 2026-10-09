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

func TestConfiguredCodexHomeRejectsForbiddenPathsBeforeLookup(t *testing.T) {
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	// None of these paths exists. A filesystem-first implementation reports an
	// unavailable account instead of applying the authorization boundary.
	for _, tc := range []struct {
		name, path, reason string
	}{
		{"workspace", filepath.Join(workspace, "missing"), "separate from the workspace"},
		{"sensitive", filepath.Join(home, ".ssh", "missing"), "dedicated provider state directory"},
		{"foreign", filepath.Join(home, ".claude", "missing"), "dedicated provider state directory"},
		{"sensitive-parent", filepath.Join(home, ".local"), "dedicated provider state directory"},
		{"home", home, "dedicated provider state directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, real, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + tc.path}, workspace, home)
			if err == nil || !strings.Contains(err.Error(), tc.reason) || got != "" || real != "" {
				t.Fatalf("boundary not enforced before lookup: %q %q %v", got, real, err)
			}
		})
	}
}

func TestConfiguredCodexHomeChecksCanonicalBoundariesBeforeFollowingLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs Windows privileges")
	}
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	for _, path := range []string{home, workspace} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, target, reason string
	}{
		{"workspace", filepath.Join(workspace, "missing"), "separate from the workspace"},
		{"sensitive", filepath.Join(home, ".ssh", "missing"), "dedicated provider state directory"},
		{"foreign", filepath.Join(home, ".claude", "missing"), "dedicated provider state directory"},
		{"sensitive-parent", filepath.Join(home, ".local"), "dedicated provider state directory"},
		{"broad", string(os.PathSeparator), "dedicated provider state directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := filepath.Join(root, "account-"+tc.name)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			got, real, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + link}, workspace, home)
			if err == nil || !strings.Contains(err.Error(), tc.reason) || got != "" || real != "" {
				t.Fatalf("link target not authorized before lookup: %q %q %v", got, real, err)
			}
		})
	}
}

func TestConfiguredCodexHomeRejectsAliasedHomeAndForeignCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs Windows privileges")
	}
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	secret, outside := filepath.Join(home, ".ssh"), filepath.Join(root, "foreign-account")
	for _, path := range []string{workspace, secret, outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + secret}, workspace, alias); err == nil {
		t.Fatal("canonical HOME sensitive directory was exposed")
	}
	if err := os.Symlink(outside, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, filepath.Join(outside, "missing")} {
		if _, _, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + path}, workspace, home); err == nil || !strings.Contains(err.Error(), "dedicated provider state directory") {
			t.Fatalf("canonical foreign credentials accepted: %q %v", path, err)
		}
	}
}

func TestConfiguredCodexHomeAcceptsOutsideDirectoryAndSensitivePrefixSibling(t *testing.T) {
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	outside, sibling := filepath.Join(root, "account"), filepath.Join(home, ".ssh-account")
	for _, path := range []string{workspace, outside, sibling} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, selected := range []string{outside, sibling} {
		got, real, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + selected}, workspace, home)
		if err != nil || got != selected || real != selected {
			t.Fatalf("dedicated account rejected: %q %q %v", got, real, err)
		}
	}
}

func TestConfiguredCodexHomeRejectsNonDirectoryAndSymlinkCycleWithoutFallback(t *testing.T) {
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	for _, path := range []string{home, workspace, filepath.Join(home, ".codex")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "account-file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{file, filepath.Join(root, "missing")}
	if runtime.GOOS != "windows" {
		cycle := filepath.Join(root, "account-cycle")
		if err := os.Symlink("account-cycle", cycle); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, cycle)
	}
	oldLookup := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = oldLookup })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	for _, path := range paths {
		cmd := exec.Command("fixture")
		cmd.Dir = workspace
		cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + path}
		if wrapped, err := wrapCLICommandWithSandbox(context.Background(), "codex", cmd); err == nil || wrapped != nil {
			t.Fatalf("invalid selection fell back to the default account: %v %v", wrapped, err)
		}
	}
}

func TestConfiguredCodexHomeRelativeLinkAndAmbiguousTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs Windows privileges")
	}
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	account := filepath.Join(root, "account")
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, path := range []string{home, workspace, account, filepath.Join(a, "chosen"), filepath.Join(b, "dir"), filepath.Join(b, "chosen")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	relative := filepath.Join(home, ".codex-selected")
	if err := os.Symlink("../account", relative); err != nil {
		t.Fatal(err)
	}
	got, real, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + relative}, workspace, home)
	if err != nil || got != relative || real != account {
		t.Fatalf("leading parent account alias rejected: %q %q %v", got, real, err)
	}
	if err := os.Symlink(filepath.Join(b, "dir"), filepath.Join(a, "link")); err != nil {
		t.Fatal(err)
	}
	ambiguous := filepath.Join(root, "ambiguous-account")
	// This physically resolves to b/chosen; cleaning the target first would
	// silently select a/chosen, potentially exposing a different account.
	if err := os.Symlink(a+"/link/../chosen", ambiguous); err != nil {
		t.Fatal(err)
	}
	if got, real, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + ambiguous}, workspace, home); err == nil || !strings.Contains(err.Error(), "ambiguous symbolic link traversal") || got != "" || real != "" {
		t.Fatalf("ambiguous account traversal was accepted: %q %q %v", got, real, err)
	}
	if got, real, err := cliConfiguredCodexHome([]string{"CODEX_HOME=" + a + "/link/../chosen"}, workspace, home); err == nil || !strings.Contains(err.Error(), "ambiguous path traversal") || got != "" || real != "" {
		t.Fatalf("ambiguous raw account traversal was accepted: %q %q %v", got, real, err)
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

func TestWrapCodexHomeForeignSelectionErrorsFailClosed(t *testing.T) {
	oldLookup := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = oldLookup })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	for _, path := range []string{home, workspace} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, selected := range []string{
		filepath.Join(root, "missing-account"),
		filepath.Join(workspace, "account"),
		home + "/a/link/../chosen",
	} {
		cmd := exec.Command("fixture")
		cmd.Dir = workspace
		cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + selected}
		if wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd); err == nil || wrapped != nil {
			t.Fatalf("unverified foreign account was left exposed: %q %v %v", selected, wrapped, err)
		}
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

func TestWrapCodexHomeReviewReadonlyTmpfsMountAndMultiAccountMasking(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	for _, dir := range []string{home, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	selected := filepath.Join(home, ".codex")
	if err := os.MkdirAll(selected, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(selected, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(selected, "config.toml")
	if err := os.WriteFile(configPath, []byte(`model = "gpt-5"`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Multi-home sibling accounts:
	codex2 := filepath.Join(home, ".codex-2")
	if err := os.MkdirAll(codex2, 0o700); err != nil {
		t.Fatal(err)
	}
	codex3 := filepath.Join(home, ".codex-3")
	if err := os.MkdirAll(codex3, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("codex", "exec")
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "CODEX_HOME=" + selected}

	// In review (read-only) mode:
	ctx := ContextWithTask(context.Background(), TaskReview)
	wrapped, err := wrapCLICommandWithSandbox(ctx, "codex", cmd)
	if err != nil {
		t.Fatal(err)
	}

	args := strings.Join(wrapped.Args, " ")
	// 1. Selected codex home must be mounted with --tmpfs to allow SQLite WAL writes
	if !strings.Contains(args, "--tmpfs "+selected) {
		t.Fatalf("expected --tmpfs %s in wrapped args, got:\n%s", selected, args)
	}
	// 2. Auth and config must be mounted with --ro-bind
	if !strings.Contains(args, "--ro-bind "+authPath+" "+authPath) {
		t.Fatalf("expected --ro-bind %s in wrapped args, got:\n%s", authPath, args)
	}
	if !strings.Contains(args, "--ro-bind "+configPath+" "+configPath) {
		t.Fatalf("expected --ro-bind %s in wrapped args, got:\n%s", configPath, args)
	}
	// 3. Multi-home siblings .codex-2 and .codex-3 must be masked
	if !strings.Contains(args, "--tmpfs "+codex2) {
		t.Fatalf("expected unselected account %s to be masked with --tmpfs, got:\n%s", codex2, args)
	}
	if !strings.Contains(args, "--tmpfs "+codex3) {
		t.Fatalf("expected unselected account %s to be masked with --tmpfs, got:\n%s", codex3, args)
	}
}
