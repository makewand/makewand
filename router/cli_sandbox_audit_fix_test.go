package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func auditFixMockBwrap(t *testing.T) {
	t.Helper()
	oldLookup := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = oldLookup })
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
}

func auditFixMissingBwrap(t *testing.T) {
	t.Helper()
	oldLookup := cliBwrapLookup
	t.Cleanup(func() { cliBwrapLookup = oldLookup })
	cliBwrapLookup = func(string) (string, error) { return "", errors.New("exec: not found") }
}

func auditFixRequireLiveBwrap(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native bubblewrap requires Linux")
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
}

func auditFixHasTriple(args []string, flag, src, dest string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == src && args[i+2] == dest {
			return true
		}
	}
	return false
}

// auditFixWaitPrivateHome runs cmd and waits for the private provider home
// to be written back and removed.
func auditFixRunAndWaitPrivateHome(t *testing.T, wrapped *exec.Cmd) string {
	t.Helper()
	finished := make(chan string, 4)
	hook := func(dir string) { finished <- dir }
	old := cliPrivateHomeFinished.Swap(&hook)
	t.Cleanup(func() { cliPrivateHomeFinished.Store(old) })
	out, err := runWrappedCLIForTest(wrapped)
	if err != nil {
		t.Fatalf("sandbox run failed: %v %s (args=%v)", err, out, wrapped.Args)
	}
	select {
	case dir := <-finished:
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("private home %s was not removed: %v", dir, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("private home write-back did not run after the sandbox exited")
	}
	return string(out)
}

// H1: no task type runs on the host without host-exec authorization.
func TestAuditFix_HostExecRequiresAuthorizationForEveryTask(t *testing.T) {
	auditFixMissingBwrap(t)
	t.Setenv("MAKEWAND_RESTRICTED", "")
	t.Setenv("MAKEWAND_REQUIRE_BWRAP", "")
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir()) // no on-disk acknowledgment
	tasks := []TaskType{TaskExplain, TaskAnalyze, TaskReview, TaskCode, TaskFix}
	for _, unsafeEnv := range []string{"", "1"} {
		t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", unsafeEnv)
		for _, task := range tasks {
			obs := &CLIExecObserver{}
			ctx := ContextWithCLIExecObserver(ContextWithTask(context.Background(), task), obs)
			cmd := exec.Command("claude", "-p", "x")
			if wrapped, err := wrapCLICommandWithSandbox(ctx, "claude", cmd); err == nil || wrapped != nil {
				t.Fatalf("task %v (MAKEWAND_UNSAFE_HOST_EXEC=%q) ran unsandboxed without acknowledgment", task, unsafeEnv)
			}
			if obs.Record().UnsafeHost {
				t.Fatalf("task %v recorded a host launch that was refused", task)
			}
		}
	}

	// Authorized: every task runs on the host, is audited and recorded.
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	audits := 0
	auth := UnsafeHostExecAuth{Acknowledged: true, Source: "test", Audit: func(string, []string, string) { audits++ }}
	for _, task := range tasks {
		obs := &CLIExecObserver{}
		ctx := ContextWithCLIExecObserver(ContextWithUnsafeHostExecAuth(ContextWithTask(context.Background(), task), auth), obs)
		cmd := exec.Command("claude", "-p", "x")
		if wrapped, err := wrapCLICommandWithSandbox(ctx, "claude", cmd); err != nil || wrapped != cmd {
			t.Fatalf("authorized task %v: %v %v", task, wrapped, err)
		}
		if rec := obs.Record(); !rec.UnsafeHost || rec.Sandboxed {
			t.Fatalf("authorized task %v not recorded as host execution: %+v", task, rec)
		}
	}
	if audits != len(tasks) {
		t.Fatalf("audits = %d, want %d", audits, len(tasks))
	}

	// Restricted and remote contexts stay closed even when authorized.
	authCtx := ContextWithUnsafeHostExecAuth(context.Background(), auth)
	if _, err := wrapCLICommandWithSandbox(ContextWithRemoteOrigin(authCtx), "claude", exec.Command("claude")); err == nil {
		t.Fatal("remote origin ran on host")
	}
	t.Setenv("MAKEWAND_RESTRICTED", "1")
	if _, err := wrapCLICommandWithSandbox(authCtx, "claude", exec.Command("claude")); err == nil {
		t.Fatal("restricted context ran on host")
	}
}

// H3: host provider directories are never bound writable.
func TestAuditFix_ProviderStateNeverBoundWritable(t *testing.T) {
	auditFixMockBwrap(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{".claude", ".codex", ".gemini", ".agy", ".grok", ".aider"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"t":1}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"t":1}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".gemini", "oauth_creds.json"), []byte(`{"t":1}`), 0o600)
	_ = os.WriteFile(filepath.Join(home, ".gemini", "settings.json"), []byte(`{}`), 0o600)
	ws := t.TempDir()
	for _, provider := range []string{"claude", "codex", "gemini", "agy", "grok", "aider"} {
		cmd := exec.Command("/usr/bin/true")
		cmd.Dir = ws
		wrapped, err := wrapCLICommandWithSandbox(context.Background(), provider, cmd)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		for i := 0; i+2 < len(wrapped.Args); i++ {
			if wrapped.Args[i] == "--bind" && cliPathWithin(home, wrapped.Args[i+1]) {
				t.Fatalf("%s: host home path %s bound writable: %v", provider, wrapped.Args[i+1], wrapped.Args)
			}
		}
		priv := cliPrivateHomeSource(t, wrapped.Args, home)
		switch provider {
		case "claude":
			if data, _ := os.ReadFile(filepath.Join(priv, ".claude", ".credentials.json")); string(data) != `{"t":1}` {
				t.Fatalf("claude credentials not copied: %q", data)
			}
		case "codex":
			if data, _ := os.ReadFile(filepath.Join(priv, ".codex", "auth.json")); string(data) != `{"t":1}` {
				t.Fatalf("codex credentials not copied: %q", data)
			}
		case "gemini", "agy":
			settings := filepath.Join(home, ".gemini", "settings.json")
			if !auditFixHasTriple(wrapped.Args, "--ro-bind", settings, settings) {
				t.Fatalf("%s: gemini settings must be bound read-only: %v", provider, wrapped.Args)
			}
			agy := filepath.Join(home, ".agy")
			if !auditFixHasTriple(wrapped.Args, "--ro-bind", agy, agy) {
				t.Fatalf("%s: config-only provider dir must be bound read-only: %v", provider, wrapped.Args)
			}
			if _, err := os.Stat(filepath.Join(priv, ".gemini", "settings.json")); err == nil {
				t.Fatalf("%s: configuration must not be copied into the writable private home", provider)
			}
		}
	}
}

// H3: changed token files are written back atomically; configuration,
// copy-only files and new files never reach the host.
func TestAuditFix_PrivateHomeLiveWriteBack(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	home, ws := t.TempDir(), t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(claudeDir, ".credentials.json")
	settings := filepath.Join(claudeDir, "settings.json")
	claudeJSON := filepath.Join(home, ".claude.json")
	for path, content := range map[string]string{creds: `{"token":"old"}`, //nolint:gosec // G101: fake fixture token
		settings: `{"hooks":{}}`, claudeJSON: `{"mcpServers":{}}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `printf '{"token":"new"}' > "$HOME/.claude/.credentials.json"
printf '{"mcpServers":{"evil":{}}}' > "$HOME/.claude.json"
echo planted > "$HOME/.claude/hooks.sh"
if printf x > "$HOME/.claude/settings.json" 2>/dev/null; then echo SETTINGS_WRITABLE; fi
echo done`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = ws
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	out := auditFixRunAndWaitPrivateHome(t, wrapped)
	if strings.Contains(out, "SETTINGS_WRITABLE") || !strings.Contains(out, "done") {
		t.Fatalf("unexpected sandbox output: %q", out)
	}
	if data, _ := os.ReadFile(creds); string(data) != `{"token":"new"}` {
		t.Fatalf("refreshed token not written back: %q", data)
	}
	if fi, err := os.Stat(creds); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("written-back credential mode: %v %v", fi, err)
	}
	if data, _ := os.ReadFile(claudeJSON); string(data) != `{"mcpServers":{}}` {
		t.Fatalf("copy-only .claude.json reached the host: %q", data)
	}
	if data, _ := os.ReadFile(settings); string(data) != `{"hooks":{}}` {
		t.Fatalf("settings changed on host: %q", data)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "hooks.sh")); !os.IsNotExist(err) {
		t.Fatalf("file planted in the private home reached the host: %v", err)
	}
}

// H3: write-back validation rejects symlinks, invalid JSON, oversized data and
// a host file that changed since launch.
func TestAuditFix_PrivateHomeWriteBackValidation(t *testing.T) {
	newHome := func(t *testing.T) (*cliPrivateHome, string, string) {
		t.Helper()
		hostRoot := t.TempDir()
		hostFile := filepath.Join(hostRoot, "auth.json")
		if err := os.WriteFile(hostFile, []byte(`{"v":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		h, err := newCLIPrivateHome(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.cleanup)
		privRoot := filepath.Join(h.dir, "codex-home")
		if err := os.Mkdir(privRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := h.copyIn(hostRoot, privRoot, cliPrivateCredFile{"auth.json", true}); err != nil {
			t.Fatal(err)
		}
		return h, hostFile, filepath.Join(privRoot, "auth.json")
	}
	read := func(path string) string { data, _ := os.ReadFile(path); return string(data) }

	t.Run("changed", func(t *testing.T) {
		h, host, priv := newHome(t)
		_ = os.WriteFile(priv, []byte(`{"v":2}`), 0o600)
		h.writeBack()
		if read(host) != `{"v":2}` {
			t.Fatalf("valid change not written back: %q", read(host))
		}
	})
	t.Run("symlink", func(t *testing.T) {
		h, host, priv := newHome(t)
		secret := filepath.Join(t.TempDir(), "secret.json")
		_ = os.WriteFile(secret, []byte(`{"secret":true}`), 0o600)
		_ = os.Remove(priv)
		if err := os.Symlink(secret, priv); err != nil {
			t.Fatal(err)
		}
		h.writeBack()
		if read(host) != `{"v":1}` {
			t.Fatalf("symlinked private credential was followed: %q", read(host))
		}
	})
	t.Run("symlinked parent", func(t *testing.T) {
		h, host, priv := newHome(t)
		other := t.TempDir()
		_ = os.WriteFile(filepath.Join(other, "auth.json"), []byte(`{"v":9}`), 0o600)
		_ = os.RemoveAll(filepath.Dir(priv))
		if err := os.Symlink(other, filepath.Dir(priv)); err != nil {
			t.Fatal(err)
		}
		h.writeBack()
		if read(host) != `{"v":1}` {
			t.Fatalf("symlinked private parent was followed: %q", read(host))
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		h, host, priv := newHome(t)
		_ = os.WriteFile(priv, []byte(`not json`), 0o600)
		h.writeBack()
		if read(host) != `{"v":1}` {
			t.Fatalf("invalid JSON written back: %q", read(host))
		}
	})
	t.Run("oversized", func(t *testing.T) {
		h, host, priv := newHome(t)
		_ = os.WriteFile(priv, []byte(`"`+strings.Repeat("x", cliPrivateCredMaxBytes)+`"`), 0o600)
		h.writeBack()
		if read(host) != `{"v":1}` {
			t.Fatal("oversized credential written back")
		}
	})
	t.Run("host changed concurrently", func(t *testing.T) {
		h, host, priv := newHome(t)
		_ = os.WriteFile(host, []byte(`{"v":"host"}`), 0o600)
		_ = os.WriteFile(priv, []byte(`{"v":2}`), 0o600)
		h.writeBack()
		if read(host) != `{"v":"host"}` {
			t.Fatalf("concurrent host refresh clobbered: %q", read(host))
		}
	})
}

// H4: git metadata inside a writable workspace is bound read-only, including
// gitfile targets, their commondir and .git/modules.
func TestAuditFix_GitMetadataReadOnlyInWritableWorkspace(t *testing.T) {
	auditFixMockBwrap(t)
	ws := t.TempDir()
	mainGit := filepath.Join(ws, ".git")
	modules := filepath.Join(mainGit, "modules", "sub")
	external := filepath.Join(ws, "meta", "wt-gitdir")
	common := filepath.Join(ws, "meta", "common")
	for _, dir := range []string{modules, filepath.Join(ws, "sub"), filepath.Join(ws, "wt"), external, common, filepath.Join(ws, "node_modules", "pkg", ".git")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	subGitFile := filepath.Join(ws, "sub", ".git")
	_ = os.WriteFile(subGitFile, []byte("gitdir: ../.git/modules/sub\n"), 0o600)
	wtGitFile := filepath.Join(ws, "wt", ".git")
	_ = os.WriteFile(wtGitFile, []byte("gitdir: "+external+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(external, "commondir"), []byte("../common\n"), 0o600)
	_ = os.Mkdir(filepath.Join(external, "modules"), 0o700)

	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = ws
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	wsBind := -1
	for i := 0; i+2 < len(wrapped.Args); i++ {
		if wrapped.Args[i] == "--bind" && wrapped.Args[i+1] == ws {
			wsBind = i
		}
	}
	for _, p := range []string{mainGit, subGitFile, wtGitFile, external, common, filepath.Join(external, "modules"), filepath.Join(ws, "node_modules", "pkg", ".git")} {
		found := false
		for i := wsBind + 1; i+2 < len(wrapped.Args); i++ {
			if wrapped.Args[i] == "--ro-bind" && wrapped.Args[i+1] == p && wrapped.Args[i+2] == p {
				found = true
			}
		}
		if !found {
			t.Errorf("git metadata %s not bound read-only after the workspace bind: %v", p, wrapped.Args)
		}
	}

	// A symlinked .git cannot be pinned by a mount: writable sessions refuse.
	linked := t.TempDir()
	if err := os.Symlink(mainGit, filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("/usr/bin/true")
	cmd.Dir = linked
	if _, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd); err == nil || !strings.Contains(err.Error(), "symlinked .git") {
		t.Fatalf("symlinked .git accepted in writable session: %v", err)
	}
	if _, err := wrapCLICommandWithSandbox(ContextWithTask(context.Background(), TaskReview), "claude", cmd); err != nil {
		t.Fatalf("read-only review of a symlinked .git workspace refused: %v", err)
	}
}

func TestAuditFix_GitMetadataLiveReadOnly(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("[core]\n"), 0o600)
	script := `if printf '[core]\nhooksPath=/tmp/x\n' >> .git/config 2>/dev/null; then echo GIT_CONFIG_WRITABLE; fi
if echo evil > .git/hooks/pre-commit 2>/dev/null; then echo GIT_HOOK_WRITABLE; fi
echo ok > file.txt && echo WS_WRITABLE`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = ws
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	out := auditFixRunAndWaitPrivateHome(t, wrapped)
	if strings.Contains(out, "GIT_") || !strings.Contains(out, "WS_WRITABLE") {
		t.Fatalf("git metadata writable or workspace read-only: %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); string(data) != "[core]\n" {
		t.Fatalf(".git/config modified: %q", data)
	}
}

// H5: unix sockets anywhere in the workspace (node_modules included) are masked.
func TestAuditFix_WorkspaceSocketsMasked(t *testing.T) {
	auditFixMockBwrap(t)
	ws, err := os.MkdirTemp("", "mwsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws) })
	ws, _ = filepath.EvalSymlinks(ws)
	sockDir := filepath.Join(ws, "node_modules", "p")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(sockDir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	for _, ctx := range []context.Context{context.Background(), ContextWithTask(context.Background(), TaskReview)} {
		cmd := exec.Command("/usr/bin/true")
		cmd.Dir = ws
		wrapped, err := wrapCLICommandWithSandbox(ctx, "claude", cmd)
		if err != nil {
			t.Fatal(err)
		}
		if !auditFixHasTriple(wrapped.Args, "--ro-bind", os.DevNull, sock) {
			t.Fatalf("workspace socket %s not masked: %v", sock, wrapped.Args)
		}
	}
	if runtime.GOOS == "linux" {
		if bwrap, err := exec.LookPath("bwrap"); err == nil && exec.Command(bwrap, "--ro-bind", "/", "/", "--", "/usr/bin/true").Run() == nil {
			cliBwrapLookup = exec.LookPath
			cmd := exec.Command("/bin/sh", "-c", `if test -S node_modules/p/s.sock; then echo SOCKET_VISIBLE; else echo MASKED; fi`)
			cmd.Dir = ws
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
			wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
			if err != nil {
				t.Fatal(err)
			}
			if out := auditFixRunAndWaitPrivateHome(t, wrapped); !strings.Contains(out, "MASKED") {
				t.Fatalf("socket reachable inside sandbox: %q", out)
			}
		}
	}
}

// H4/H5: a truncated workspace scan fails closed for writable sessions.
func TestAuditFix_TruncatedWorkspaceScanFailsClosed(t *testing.T) {
	auditFixMockBwrap(t)
	old := cliWorkspaceScanLimit
	cliWorkspaceScanLimit = 5
	t.Cleanup(func() { cliWorkspaceScanLimit = old })
	ws := t.TempDir()
	for i := 0; i < 10; i++ {
		_ = os.WriteFile(filepath.Join(ws, "f"+strconv.Itoa(i)), nil, 0o600)
	}
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = ws
	if _, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd); err == nil || !strings.Contains(err.Error(), "too large to verify") {
		t.Fatalf("truncated scan did not fail closed: %v", err)
	}
	if _, err := wrapCLICommandWithSandbox(ContextWithTask(context.Background(), TaskReview), "claude", cmd); err != nil {
		t.Fatalf("read-only review must not need a complete scan: %v", err)
	}
}

// H6: the child environment is an explicit allowlist.
func TestAuditFix_SanitizeCLIEnvTightened(t *testing.T) {
	withoutCLITestHooks(t)
	env := []string{
		"PATH=/usr/bin", "HOME=/home/u", "NODE_ENV=production", "GOPATH=/go",
		"LD_PRELOAD=/tmp/evil.so", "LD_LIBRARY_PATH=/tmp", "LD_AUDIT=/tmp/a.so",
		"NODE_OPTIONS=--require /tmp/evil.js", "PYTHONSTARTUP=/tmp/evil.py",
		"SAFE_THING=1", "TEST_THING=1",
		"MAKEWAND_UNSAFE_HOST_EXEC=1", "MAKEWAND_CONFIG_DIR=/x", "MAKEWAND_REMOTE_TOKEN=tok",
		"MAKEWAND_CLI_PROCESS_MARKER=/tmp/m", "MAKEWAND_TASK_ID=task-1", "MAKEWAND_WORKSPACE_ID=ws-1",
		"CODEX_HOME=/c", "OPENAI_API_KEY=sk-o", "ANTHROPIC_API_KEY=sk-a",
	}
	got := map[string]bool{}
	for _, entry := range sanitizeCLIEnv("codex", env) {
		got[strings.SplitN(entry, "=", 2)[0]] = true
	}
	for _, key := range []string{"PATH", "HOME", "NODE_ENV", "GOPATH", "MAKEWAND_TASK_ID", "MAKEWAND_WORKSPACE_ID", "CODEX_HOME", "OPENAI_API_KEY"} {
		if !got[key] {
			t.Errorf("%s must pass through, got %v", key, got)
		}
	}
	for _, key := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "NODE_OPTIONS", "PYTHONSTARTUP", "SAFE_THING", "TEST_THING",
		"MAKEWAND_UNSAFE_HOST_EXEC", "MAKEWAND_CONFIG_DIR", "MAKEWAND_REMOTE_TOKEN", "MAKEWAND_CLI_PROCESS_MARKER", "ANTHROPIC_API_KEY"} {
		if got[key] {
			t.Errorf("%s must be dropped, got %v", key, got)
		}
	}
}

// M-broad: HOME, its ancestors and the broad roots are refused for every task,
// and the host-root masks always apply.
func TestAuditFix_BroadWorkspaceRefused(t *testing.T) {
	auditFixMockBwrap(t)
	root := t.TempDir()
	home := filepath.Join(root, "users", "me")
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{home, filepath.Join(root, "users"), root, link} {
		if !isForbiddenWritableWorkspace(ws, home) {
			t.Errorf("%s (HOME or ancestor) must be forbidden", ws)
		}
	}
	if isForbiddenWritableWorkspace(project, home) {
		t.Errorf("project below HOME must be allowed")
	}
	for _, ws := range []string{"/var/tmp", "/mnt", "/srv", "/media"} {
		if !isForbiddenWritableWorkspace(ws) {
			t.Errorf("%s must be forbidden", ws)
		}
	}

	// The child's HOME counts even when the process HOME differs.
	for _, task := range []TaskType{TaskCode, TaskReview, TaskExplain} {
		cmd := exec.Command("/usr/bin/true")
		cmd.Dir = filepath.Join(root, "users")
		cmd.Env = []string{"PATH=/usr/bin", "HOME=" + home}
		if _, err := wrapCLICommandWithSandbox(ContextWithTask(context.Background(), task), "claude", cmd); err == nil || !strings.Contains(err.Error(), "refusing to use") {
			t.Fatalf("task %v: ancestor of child HOME accepted: %v", task, err)
		}
	}

	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = project
	cmd.Env = []string{"PATH=/usr/bin", "HOME=" + home}
	wrapped, err := wrapCLICommandWithSandbox(ContextWithTask(context.Background(), TaskReview), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(wrapped.Args, " ")
	for _, r := range []string{"/root", "/media", "/srv", "/mnt", "/home"} {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() && !strings.Contains(args, "--tmpfs "+r+" ") {
			t.Errorf("host root %s not masked: %v", r, wrapped.Args)
		}
	}
}

// M-seccomp: the filter is mandatory by default on amd64/arm64.
func TestAuditFix_SeccompRequiredByDefault(t *testing.T) {
	defaultArch := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	cases := []struct {
		require, allow     string
		required, optedOut bool
	}{
		{"", "", defaultArch, false},
		{"", "1", false, defaultArch},
		{"1", "1", true, false},
		{"1", "", true, false},
	}
	for _, c := range cases {
		t.Setenv("MAKEWAND_REQUIRE_SECCOMP", c.require)
		t.Setenv("MAKEWAND_ALLOW_NO_SECCOMP", c.allow)
		if req, opt := cliSeccompPolicy(); req != c.required || opt != c.optedOut {
			t.Errorf("REQUIRE=%q ALLOW=%q: got (%v,%v), want (%v,%v)", c.require, c.allow, req, opt, c.required, c.optedOut)
		}
	}
	if !defaultArch {
		return
	}
	t.Setenv("MAKEWAND_REQUIRE_SECCOMP", "")
	t.Setenv("MAKEWAND_ALLOW_NO_SECCOMP", "")
	auditFixMockBwrap(t)
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = t.TempDir()
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(wrapped.Args, " "), "--seccomp ") {
		t.Fatalf("seccomp filter missing by default: %v", wrapped.Args)
	}
}

// Test-fixture binds are a _test.go hook only: production plans never bind a
// /tmp/Test* root or a HOME under /tmp writable from the host.
func TestAuditFix_NoTestFixtureBindsInProduction(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cli_sandbox.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		if imp.Path.Value == `"testing"` {
			t.Fatal("cli_sandbox.go imports testing")
		}
	}

	withoutCLITestHooks(t)
	auditFixMockBwrap(t)
	fixture := t.TempDir()
	if !strings.HasPrefix(fixture, "/tmp/Test") {
		t.Skipf("t.TempDir outside /tmp: %s", fixture)
	}
	fixtureRoot := filepath.Join("/tmp", strings.Split(strings.TrimPrefix(fixture, "/tmp/"), "/")[0])
	bin := filepath.Join(fixture, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test fake executable
		t.Fatal(err)
	}
	home := filepath.Join(fixture, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-p", "x") //nolint:gosec // G204: test-owned fake executable
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=/usr/bin", "HOME=" + home, "MAKEWAND_CLI_PROCESS_MARKER=" + filepath.Join(fixture, "m")}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+2 < len(wrapped.Args); i++ {
		if wrapped.Args[i] == "--bind" && (wrapped.Args[i+1] == fixtureRoot || wrapped.Args[i+1] == home) {
			t.Fatalf("production plan bound %s writable: %v", wrapped.Args[i+1], wrapped.Args)
		}
	}
	cliPrivateHomeSource(t, wrapped.Args, home)
	if strings.Contains(fmt.Sprint(wrapped.Env), "MAKEWAND_CLI_PROCESS_MARKER") {
		t.Fatal("test marker variable passed in production configuration")
	}
}

// Every provider CLI child carries MAKEWAND_PROVIDER_WORKER=1, on the
// sandboxed path (bwrap --setenv and the wrapper env) and the authorized host
// path, so the makewand CLI cannot recurse into model dispatch.
func TestAuditFix_ProviderWorkerEnvSet(t *testing.T) {
	withoutCLITestHooks(t)
	hasWorker := func(env []string) bool {
		n := 0
		for _, e := range env {
			if strings.HasPrefix(e, "MAKEWAND_PROVIDER_WORKER=") {
				if e != "MAKEWAND_PROVIDER_WORKER=1" {
					return false
				}
				n++
			}
		}
		return n == 1
	}
	env := []string{"PATH=/usr/bin", "HOME=" + t.TempDir(), "MAKEWAND_PROVIDER_WORKER=0"}

	auditFixMockBwrap(t)
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir, cmd.Env = t.TempDir(), env
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWorker(wrapped.Env) || !auditFixHasTriple(wrapped.Args, "--setenv", "MAKEWAND_PROVIDER_WORKER", "1") {
		t.Fatalf("sandboxed child lacks MAKEWAND_PROVIDER_WORKER=1: env=%v args=%v", wrapped.Env, wrapped.Args)
	}
	for i := 0; i+2 < len(wrapped.Args); i++ {
		if wrapped.Args[i] == "--setenv" && wrapped.Args[i+1] == "MAKEWAND_PROVIDER_WORKER" && wrapped.Args[i+2] != "1" {
			t.Fatalf("inherited MAKEWAND_PROVIDER_WORKER value leaked: %v", wrapped.Args)
		}
	}

	auditFixMissingBwrap(t)
	t.Setenv("MAKEWAND_RESTRICTED", "")
	t.Setenv("MAKEWAND_REQUIRE_BWRAP", "")
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	ctx := ContextWithUnsafeHostExecAuth(context.Background(), UnsafeHostExecAuth{Acknowledged: true, Source: "test", Audit: func(string, []string, string) {}})
	host := exec.Command("/usr/bin/true")
	host.Env = env
	got, err := wrapCLICommandWithSandbox(ctx, "codex", host)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWorker(got.Env) {
		t.Fatalf("host child lacks MAKEWAND_PROVIDER_WORKER=1: %v", got.Env)
	}
}

// runWrappedCLIForTest mirrors the production start sequence: Start, then
// cliAfterStart on the same goroutine, then Wait.
func runWrappedCLIForTest(cmd *exec.Cmd) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	cliAfterStart(cmd)
	err := cmd.Wait()
	return buf.Bytes(), err
}

func TestAuditFix_TmpProviderBinaryDirBoundReadOnly(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mw-bin-")
	if err != nil {
		t.Skip("no /tmp")
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: fixture must be executable
		t.Fatal(err)
	}
	args := rebindToolchainUnderMaskedRoot(nil, bin, t.TempDir(), "")
	if !cliHasMount(args, "--ro-bind", dir) {
		t.Fatalf("binary dir %s not ro-bound: %v", dir, args)
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--bind" || args[i+1] == "/tmp" {
			t.Fatalf("unexpected writable or /tmp mount: %v", args)
		}
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if args := rebindToolchainUnderMaskedRoot(nil, bin, t.TempDir(), ""); cliHasMount(args, "--ro-bind", dir) {
		t.Fatalf("world-writable dir must not be exposed: %v", args)
	}
}

func cliHasMount(args []string, flag, path string) bool {
	for i := 0; i+2 < len(args)+1; i++ {
		if i+1 < len(args) && args[i] == flag && args[i+1] == path {
			return true
		}
	}
	return false
}
