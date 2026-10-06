package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWrapCLICommandWithSandbox_WrapsWithBwrap(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "hello")
	cmd.Dir = ws

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}
	if wrapped.Path != "/usr/bin/bwrap" {
		t.Fatalf("wrapped.Path = %q, want /usr/bin/bwrap", wrapped.Path)
	}

	args := strings.Join(wrapped.Args, " ")
	if strings.Contains(args, "--unshare-net") {
		t.Errorf("CLI sandbox must NOT unshare net, but found --unshare-net in %v", wrapped.Args)
	}
	if !strings.Contains(args, "--bind "+ws+" "+ws) {
		t.Errorf("expected workspace to be bound read-write: %v", wrapped.Args)
	}
	if !strings.Contains(args, "--die-with-parent") {
		t.Errorf("missing --die-with-parent in %v", wrapped.Args)
	}
	if !strings.Contains(args, "--tmpfs /tmp") {
		t.Errorf("missing --tmpfs /tmp in %v", wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_ReviewTaskReadOnly(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	ws := t.TempDir()
	cmd := exec.Command("codex", "exec")
	cmd.Dir = ws

	ctx := ContextWithTask(context.Background(), TaskReview)
	wrapped, err := wrapCLICommandWithSandbox(ctx, "codex", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")
	if strings.Contains(args, "--bind "+ws+" "+ws) {
		t.Errorf("review task should not bind workspace read-write: %v", wrapped.Args)
	}
	if !strings.Contains(args, "--ro-bind "+ws+" "+ws) {
		t.Errorf("expected review task to bind workspace read-only: %v", wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_ActiveProviderCredentialsPreserved(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Create fake credential files and directories on host
	_ = os.MkdirAll(filepath.Join(fakeHome, ".ssh"), 0o700)
	_ = os.MkdirAll(filepath.Join(fakeHome, ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(fakeHome, ".claude.json"), []byte("{}"), 0o600)
	_ = os.MkdirAll(filepath.Join(fakeHome, ".codex"), 0o700)
	_ = os.MkdirAll(filepath.Join(fakeHome, ".gemini"), 0o700)

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	// For claude provider: .ssh, .codex, .gemini must be masked; .claude, .claude.json preserved.
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")
	sshPath := filepath.Join(fakeHome, ".ssh")
	codexPath := filepath.Join(fakeHome, ".codex")
	geminiPath := filepath.Join(fakeHome, ".gemini")
	claudePath := filepath.Join(fakeHome, ".claude")
	claudeJSONPath := filepath.Join(fakeHome, ".claude.json")

	if !strings.Contains(args, "--tmpfs "+sshPath) {
		t.Errorf("expected .ssh to be masked: %v", wrapped.Args)
	}
	if !strings.Contains(args, "--tmpfs "+codexPath) {
		t.Errorf("expected .codex to be masked for claude provider: %v", wrapped.Args)
	}
	if !strings.Contains(args, "--tmpfs "+geminiPath) {
		t.Errorf("expected .gemini to be masked for claude provider: %v", wrapped.Args)
	}
	if strings.Contains(args, "--tmpfs "+claudePath) {
		t.Errorf("active provider credentials (%s) should NOT be masked with tmpfs: %v", claudePath, wrapped.Args)
	}
	if strings.Contains(args, "--tmpfs "+claudeJSONPath) {
		t.Errorf("active provider credentials (%s) should NOT be masked with tmpfs: %v", claudeJSONPath, wrapped.Args)
	}
	if !strings.Contains(args, "--bind "+claudePath+" "+claudePath) {
		t.Errorf("expected active provider credentials (%s) to be bound: %v", claudePath, wrapped.Args)
	}
	if !strings.Contains(args, "--bind "+claudeJSONPath+" "+claudeJSONPath) {
		t.Errorf("expected active provider credentials (%s) to be bound: %v", claudeJSONPath, wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_FailCloseWhenBwrapMissing(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "", errors.New("exec: not found")
	}

	cmd := exec.Command("claude", "-p", "test")

	// 1. Normal trusted context -> fallback gracefully
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("trusted context should fallback without error: %v", err)
	}
	if wrapped != cmd {
		t.Errorf("expected original cmd on fallback, got %v", wrapped)
	}

	// 2. Remote origin -> fail close
	remoteCtx := ContextWithRemoteOrigin(context.Background())
	_, err = wrapCLICommandWithSandbox(remoteCtx, "claude", cmd)
	if err == nil {
		t.Fatal("expected error in remote origin context when bwrap is missing, got nil")
	}

	// 3. TaskReview -> fail close
	reviewCtx := ContextWithTask(context.Background(), TaskReview)
	_, err = wrapCLICommandWithSandbox(reviewCtx, "claude", cmd)
	if err == nil {
		t.Fatal("expected error in TaskReview context when bwrap is missing, got nil")
	}

	// 4. MAKEWAND_RESTRICTED=1 -> fail close
	t.Setenv("MAKEWAND_RESTRICTED", "1")
	_, err = wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err == nil {
		t.Fatal("expected error with MAKEWAND_RESTRICTED=1 when bwrap is missing, got nil")
	}
	t.Setenv("MAKEWAND_RESTRICTED", "")

	// 5. MAKEWAND_REQUIRE_BWRAP=1 -> fail close
	t.Setenv("MAKEWAND_REQUIRE_BWRAP", "1")
	_, err = wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err == nil {
		t.Fatal("expected error with MAKEWAND_REQUIRE_BWRAP=1 when bwrap is missing, got nil")
	}
}

func TestWrapCLICommandWithSandbox_LiveExecution(t *testing.T) {
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not installed")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	secretDir := filepath.Join(fakeHome, ".ssh")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secretFile, []byte("SUPER_SECRET_KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	script := `import os
if os.path.exists("` + secretFile + `"):
    print("LEAKED")
else:
    print("SAFE")
`
	cmd := exec.Command("python3", "-c", script)
	cmd.Dir = ws

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	out, err := wrapped.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapped bwrap run failed: %v (output: %q), bwrapPath=%s, args=%v", err, string(out), bwrapPath, wrapped.Args)
	}
	if !strings.Contains(string(out), "SAFE") {
		t.Fatalf("secret was not masked inside bwrap: %s", string(out))
	}
}

func TestWrapCLICommandWithSandbox_WorkspaceIsHome(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	_ = os.MkdirAll(filepath.Join(fakeHome, ".ssh"), 0o700)

	// Workspace IS HOME
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = fakeHome

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")
	sshPath := filepath.Join(fakeHome, ".ssh")
	if !strings.Contains(args, "--tmpfs "+sshPath) {
		t.Fatalf("expected .ssh to be masked even when workspace is HOME, got args: %v", wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_SymlinkWorkspace(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	realDir := t.TempDir()
	slinkDir := filepath.Join(t.TempDir(), "slink")
	if err := os.Symlink(realDir, slinkDir); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = slinkDir

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	canonicalReal, _ := filepath.EvalSymlinks(realDir)
	if wrapped.Dir != canonicalReal {
		t.Fatalf("wrapped.Dir = %q, want canonical %q", wrapped.Dir, canonicalReal)
	}
}

func TestWrapCLICommandWithSandbox_RemoteOriginBypassAttempt(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "", errors.New("exec: not found")
	}

	cmd := exec.Command("claude", "-p", "test")

	// Even if MAKEWAND_UNSAFE_HOST_EXEC is 1, RemoteOrigin MUST fail closed when bwrap is missing
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	remoteCtx := ContextWithRemoteOrigin(context.Background())
	_, err := wrapCLICommandWithSandbox(remoteCtx, "claude", cmd)
	if err == nil {
		t.Fatal("expected fail-close for remote origin even when MAKEWAND_UNSAFE_HOST_EXEC=1")
	}
}

func TestWrapCLICommandWithSandbox_HomeMasking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap mount layout; Windows required-sandbox refusal is exercised separately")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeUserHome := "/home/testuser"
	t.Setenv("HOME", fakeUserHome)

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")
	if !strings.Contains(args, "--tmpfs /home") {
		t.Errorf("expected /home to be masked with --tmpfs /home, got: %v", wrapped.Args)
	}
	if !strings.Contains(args, "--tmpfs "+fakeUserHome) {
		t.Errorf("expected user home %s to be masked with --tmpfs, got: %v", fakeUserHome, wrapped.Args)
	}
}

func TestSanitizeCLIEnv(t *testing.T) {
	rawEnv := []string{
		"PATH=/usr/bin:/bin",
		"LANG=en_US.UTF-8",
		"TERM=xterm-256color",
		"AWS_ACCESS_KEY_ID=fake-access-key-id",
		"AWS_SECRET_ACCESS_KEY=fake-secret-key",
		"GITHUB_TOKEN=fake-github-token",
		"DATABASE_URL=postgres://demo:dummy@localhost:5432/db",
		"MY_CUSTOM_PASSWORD=secret",
		"ANTHROPIC_API_KEY=sk-ant-testkey",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"OPENAI_API_KEY=sk-openaikey",
		"CODEX_HOME=/custom/codex_home",
		"GEMINI_API_KEY=aizageminikey",
	}

	// 1. For claude: ANTHROPIC_API_KEY and CLAUDE_CODE_ENTRYPOINT should be preserved,
	// foreign secrets/keys filtered out.
	claudeEnv := sanitizeCLIEnv("claude", rawEnv)
	claudeStr := strings.Join(claudeEnv, "\n")
	if !strings.Contains(claudeStr, "ANTHROPIC_API_KEY=sk-ant-testkey") {
		t.Errorf("expected ANTHROPIC_API_KEY preserved for claude, got:\n%s", claudeStr)
	}
	if !strings.Contains(claudeStr, "CLAUDE_CODE_ENTRYPOINT=cli") {
		t.Errorf("expected CLAUDE_CODE_ENTRYPOINT preserved for claude, got:\n%s", claudeStr)
	}
	if !strings.Contains(claudeStr, "PATH=/usr/bin:/bin") || !strings.Contains(claudeStr, "LANG=en_US.UTF-8") {
		t.Errorf("expected system variables preserved, got:\n%s", claudeStr)
	}
	if strings.Contains(claudeStr, "AWS_") || strings.Contains(claudeStr, "GITHUB_TOKEN") || strings.Contains(claudeStr, "DATABASE_URL") || strings.Contains(claudeStr, "PASSWORD") {
		t.Errorf("secrets not filtered: %s", claudeStr)
	}
	if strings.Contains(claudeStr, "OPENAI_API_KEY") || strings.Contains(claudeStr, "CODEX_HOME") || strings.Contains(claudeStr, "GEMINI_API_KEY") {
		t.Errorf("foreign provider keys not filtered for claude: %s", claudeStr)
	}

	// 2. For codex: OPENAI_API_KEY and CODEX_HOME should be preserved, ANTHROPIC filtered out.
	codexEnv := sanitizeCLIEnv("codex", rawEnv)
	codexStr := strings.Join(codexEnv, "\n")
	if !strings.Contains(codexStr, "OPENAI_API_KEY=sk-openaikey") || !strings.Contains(codexStr, "CODEX_HOME=/custom/codex_home") {
		t.Errorf("expected OPENAI_API_KEY and CODEX_HOME preserved for codex, got:\n%s", codexStr)
	}
	if strings.Contains(codexStr, "ANTHROPIC_API_KEY") {
		t.Errorf("ANTHROPIC_API_KEY not filtered for codex: %s", codexStr)
	}

	// 3. For gemini / agy / antigravity: GEMINI_API_KEY should be preserved, OPENAI and ANTHROPIC filtered out.
	geminiEnv := sanitizeCLIEnv("gemini", rawEnv)
	geminiStr := strings.Join(geminiEnv, "\n")
	if !strings.Contains(geminiStr, "GEMINI_API_KEY=aizageminikey") {
		t.Errorf("expected GEMINI_API_KEY preserved for gemini, got:\n%s", geminiStr)
	}
	if strings.Contains(geminiStr, "OPENAI_API_KEY") || strings.Contains(geminiStr, "ANTHROPIC_API_KEY") {
		t.Errorf("foreign provider keys not filtered for gemini: %s", geminiStr)
	}

	// 4. For antigravity: GEMINI_API_KEY should be preserved, foreign keys filtered out.
	agyEnv := sanitizeCLIEnv("antigravity", rawEnv)
	agyStr := strings.Join(agyEnv, "\n")
	if !strings.Contains(agyStr, "GEMINI_API_KEY=aizageminikey") {
		t.Errorf("expected GEMINI_API_KEY preserved for antigravity, got:\n%s", agyStr)
	}
	if strings.Contains(agyStr, "OPENAI_API_KEY") || strings.Contains(agyStr, "ANTHROPIC_API_KEY") {
		t.Errorf("foreign provider keys not filtered for antigravity: %s", agyStr)
	}
}

func TestWrapCLICommandWithSandbox_SanitizesEnv(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"AWS_SECRET_ACCESS_KEY=leak-me",
		"OPENAI_API_KEY=sk-foreign-key",
		"ANTHROPIC_API_KEY=sk-ant-valid",
	}

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	envStr := strings.Join(wrapped.Env, "\n")
	if !strings.Contains(envStr, "ANTHROPIC_API_KEY=sk-ant-valid") {
		t.Errorf("expected active provider key preserved, got:\n%s", envStr)
	}
	if strings.Contains(envStr, "AWS_SECRET_ACCESS_KEY") || strings.Contains(envStr, "OPENAI_API_KEY") {
		t.Errorf("expected secrets and foreign keys removed from wrapped.Env, got:\n%s", envStr)
	}
}
