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
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap fail-close; Windows required-sandbox refusal is exercised separately")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "", errors.New("exec: not found")
	}

	cmd := exec.Command("claude", "-p", "test")

	// 1. Normal trusted context (unspecified task or TaskCode) -> must fail closed when bwrap is missing
	_, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err == nil {
		t.Fatal("expected error in default writing context when bwrap is missing, got nil")
	}

	// 1b. Informational task (TaskExplain) -> falls back gracefully with sanitized env
	explainCtx := ContextWithTask(context.Background(), TaskExplain)
	wrappedExplain, err := wrapCLICommandWithSandbox(explainCtx, "claude", cmd)
	if err != nil {
		t.Fatalf("TaskExplain context should fallback without error: %v", err)
	}
	if wrappedExplain != cmd {
		t.Errorf("expected original cmd on TaskExplain fallback, got %v", wrappedExplain)
	}

	// 1c. Authorized host execution (MAKEWAND_UNSAFE_HOST_EXEC=1 + ack) -> fallback allowed
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	authCtx := ContextWithUnsafeHostExecAuth(context.Background(), UnsafeHostExecAuth{Acknowledged: true, Source: "test"})
	wrappedAuth, err := wrapCLICommandWithSandbox(authCtx, "claude", cmd)
	if err != nil {
		t.Fatalf("authorized host exec context should fallback without error: %v", err)
	}
	if wrappedAuth != cmd {
		t.Errorf("expected original cmd on authorized host exec fallback, got %v", wrappedAuth)
	}
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "")

	// 1d. MAKEWAND_UNSAFE_HOST_EXEC=1 WITHOUT acknowledgment -> must fail closed
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	_, err = wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err == nil {
		t.Fatal("expected error when MAKEWAND_UNSAFE_HOST_EXEC=1 without ack, got nil")
	}
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "")

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

	// 5. Server environment variables must be filtered out for all providers.
	serverEnv := []string{
		"PATH=/usr/bin:/bin",
		"MAKEWAND_SERVER_AUTH_CONFIG=/var/lib/makewand/server_auth.json",
		"MAKEWAND_DATA_DIR=/var/lib/makewand",
		"MAKEWAND_CONFIG_DIR=/var/lib/makewand/config",
		"MAKEWAND_AUTH_CONFIG=/etc/makewand/server_auth.json",
		"MAKEWAND_ADMIN_SESSION_SECRET=supersecret",
		"MAKEWAND_STATE_DB=/var/lib/makewand/state.db",
		"ANTHROPIC_API_KEY=sk-ant-testkey",
	}
	filteredServerEnv := sanitizeCLIEnv("claude", serverEnv)
	filteredStr := strings.Join(filteredServerEnv, "\n")
	for _, secretKey := range []string{
		"MAKEWAND_SERVER_AUTH_CONFIG",
		"MAKEWAND_DATA_DIR",
		"MAKEWAND_CONFIG_DIR",
		"MAKEWAND_AUTH_CONFIG",
		"MAKEWAND_ADMIN_SESSION_SECRET",
		"MAKEWAND_STATE_DB",
	} {
		if strings.Contains(filteredStr, secretKey) {
			t.Errorf("expected %s to be filtered from CLI environment, got:\n%s", secretKey, filteredStr)
		}
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

func TestWrapCLICommandWithSandbox_ProbeFailure_GracefulFallbackOrError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap probe failure; Windows required-sandbox refusal is exercised separately")
	}
	oldLookup := cliBwrapLookup
	oldProbe := cliBwrapProbe
	defer func() {
		cliBwrapLookup = oldLookup
		cliBwrapProbe = oldProbe
	}()

	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}
	cliBwrapProbe = func(path string) error {
		return errors.New("simulated userns restricted: permission denied")
	}

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	// 1. Untrusted / remote context (sandbox required) -> must fail closed with ErrorKindConfig
	untrustedCtx := ContextWithRemoteOrigin(context.Background())
	_, err := wrapCLICommandWithSandbox(untrustedCtx, "claude", cmd)
	if err == nil {
		t.Fatalf("expected error when sandbox probe fails in untrusted context, got nil")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Kind != ErrorKindConfig {
		t.Errorf("expected ErrorKindConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "simulated userns restricted") {
		t.Errorf("expected error message to contain probe failure details, got %v", err)
	}

	// 2. Informational task (TaskExplain) -> graceful fallback to unsandboxed command
	explainCtx := ContextWithTask(context.Background(), TaskExplain)
	fallbackCmd, err := wrapCLICommandWithSandbox(explainCtx, "claude", cmd)
	if err != nil {
		t.Fatalf("expected nil error on TaskExplain fallback, got %v", err)
	}
	if fallbackCmd != cmd {
		t.Errorf("expected direct execution cmd returned on fallback, got %v", fallbackCmd)
	}

	// 3. Default writing context without auth -> fails closed on probe failure
	_, err = wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err == nil {
		t.Fatalf("expected error on probe failure in default writing context, got nil")
	}
}

func TestRebindToolchainUnderMaskedRoot_NeverExposesHomeOrLocalShare(t *testing.T) {
	fakeHome := filepath.Join("/home", "testuser")
	ws := filepath.Join("/tmp", "workspace")

	// 1. ~/.local/bin/claude layout
	localBin := filepath.Join(fakeHome, ".local", "bin", "claude")
	localRoot := filepath.Join(fakeHome, ".local")
	localBinDir := filepath.Join(fakeHome, ".local", "bin")

	// cliToolchainRoot must return ~/.local/bin, NEVER ~/.local
	root := cliToolchainRoot(fakeHome, localBin)
	if root == localRoot {
		t.Fatalf("cliToolchainRoot returned ~/.local (%s), want %s", root, localBinDir)
	}
	if root != localBinDir && root != localBin {
		t.Fatalf("cliToolchainRoot = %s, want %s or %s", root, localBinDir, localBin)
	}

	// 2. ~/bin/claude layout
	homeBin := filepath.Join(fakeHome, "bin", "claude")
	homeBinDir := filepath.Join(fakeHome, "bin")

	// cliToolchainRoot must return ~/bin or ~/bin/claude, NEVER fakeHome
	root = cliToolchainRoot(fakeHome, homeBin)
	if root == fakeHome {
		t.Fatalf("cliToolchainRoot returned fakeHome (%s), want %s", root, homeBinDir)
	}
	if root != homeBinDir && root != homeBin {
		t.Fatalf("cliToolchainRoot = %s, want %s or %s", root, homeBinDir, homeBin)
	}

	// 3. Verify rebindToolchainUnderMaskedRoot never introduces --ro-bind fakeHome or --ro-bind ~/.local
	var args []string
	args = rebindToolchainUnderMaskedRoot(args, localBin, ws, fakeHome)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--ro-bind" {
			bound := args[i+1]
			if bound == fakeHome {
				t.Fatalf("rebindToolchainUnderMaskedRoot bound fakeHome: %v", args)
			}
			if bound == localRoot {
				t.Fatalf("rebindToolchainUnderMaskedRoot bound ~/.local: %v", args)
			}
		}
	}

	args = nil
	args = rebindToolchainUnderMaskedRoot(args, homeBin, ws, fakeHome)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--ro-bind" {
			bound := args[i+1]
			if bound == fakeHome {
				t.Fatalf("rebindToolchainUnderMaskedRoot bound fakeHome: %v", args)
			}
		}
	}
}

func TestWrapCLICommandWithSandbox_LocalAndHomeBinLayoutsDoNotExposeSecrets(t *testing.T) {
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeHome := filepath.Join("/home", "secuser")
	t.Setenv("HOME", fakeHome)

	ws := t.TempDir()

	// Layout A: ~/.local/bin/claude
	cmdA := exec.Command(filepath.Join(fakeHome, ".local", "bin", "claude"), "-p", "hi") //nolint:gosec // G204: test-only fake command path
	cmdA.Dir = ws
	wrappedA, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmdA)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox (local layout): %v", err)
	}
	for i := 0; i+1 < len(wrappedA.Args); i++ {
		if wrappedA.Args[i] == "--ro-bind" {
			bound := wrappedA.Args[i+1]
			if bound == fakeHome {
				t.Errorf("CLI sandbox re-bound HOME in ~/.local layout: %v", wrappedA.Args)
			}
			if bound == filepath.Join(fakeHome, ".local") {
				t.Errorf("CLI sandbox re-bound ~/.local in ~/.local layout: %v", wrappedA.Args)
			}
		}
	}

	// Layout B: ~/bin/claude
	cmdB := exec.Command(filepath.Join(fakeHome, "bin", "claude"), "-p", "hi") //nolint:gosec // G204: test-only fake command path
	cmdB.Dir = ws
	wrappedB, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmdB)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox (bin layout): %v", err)
	}
	for i := 0; i+1 < len(wrappedB.Args); i++ {
		if wrappedB.Args[i] == "--ro-bind" {
			bound := wrappedB.Args[i+1]
			if bound == fakeHome {
				t.Errorf("CLI sandbox re-bound HOME in ~/bin layout: %v", wrappedB.Args)
			}
		}
	}
}

func TestWrapCLICommandWithSandbox_SystemdServiceLayout_HomeOutsideHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap mount layout test")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	// Service layout where data-dir is /var/lib/makewand, and HOME is /var/lib/makewand/cli-home
	serviceDataDir := filepath.Join("/var", "lib", "makewand")
	serviceHome := filepath.Join(serviceDataDir, "cli-home")
	t.Setenv("HOME", serviceHome)

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")
	if !strings.Contains(args, "--tmpfs "+serviceHome) {
		t.Errorf("expected service home %s outside /home to be masked with --tmpfs, got: %v", serviceHome, wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_RemoteOriginReadOnlyCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap mount layout test")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	claudeDir := filepath.Join(fakeHome, ".claude")
	_ = os.MkdirAll(claudeDir, 0o700)
	_ = os.WriteFile(filepath.Join(fakeHome, ".claude.json"), []byte("{}"), 0o600)

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	remoteCtx := ContextWithRemoteOrigin(context.Background())
	wrapped, err := wrapCLICommandWithSandbox(remoteCtx, "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")
	// In remote origin, HOME must be masked with tmpfs even if under /tmp
	if !strings.Contains(args, "--tmpfs "+fakeHome) {
		t.Errorf("expected remote origin to mask fakeHome with --tmpfs, got: %v", wrapped.Args)
	}
	// Provider credentials must be bound with --ro-bind, not --bind
	if !strings.Contains(args, "--ro-bind "+claudeDir+" "+claudeDir) {
		t.Errorf("expected active provider credentials to be bound read-only in remote origin, got: %v", wrapped.Args)
	}
	if strings.Contains(args, "--bind "+claudeDir+" "+claudeDir) {
		t.Errorf("expected active provider credentials NOT to be bound read-write in remote origin: %v", wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_ServerDataAndSecretMasking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap mount layout test")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeDataDir := t.TempDir()
	fakeAuthFile := filepath.Join(t.TempDir(), "server_auth.json")
	_ = os.WriteFile(fakeAuthFile, []byte(`{"tokens":[]}`), 0o600)

	t.Setenv("MAKEWAND_DATA_DIR", fakeDataDir)
	t.Setenv("MAKEWAND_SERVER_AUTH_CONFIG", fakeAuthFile)

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")
	if !strings.Contains(args, "--tmpfs "+fakeDataDir) {
		t.Errorf("expected MAKEWAND_DATA_DIR %s to be masked with --tmpfs, got: %v", fakeDataDir, wrapped.Args)
	}
	if !strings.Contains(args, "--ro-bind /dev/null "+fakeAuthFile) {
		t.Errorf("expected MAKEWAND_SERVER_AUTH_CONFIG %s to be masked with /dev/null, got: %v", fakeAuthFile, wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_ProviderStateShielding(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap mount layout test")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Set up claude directory with protected, ephemeral, and project memory paths
	claudeDir := filepath.Join(fakeHome, ".claude")
	_ = os.MkdirAll(filepath.Join(claudeDir, "commands"), 0o700)
	_ = os.MkdirAll(filepath.Join(claudeDir, "skills"), 0o700)
	_ = os.MkdirAll(filepath.Join(claudeDir, "hooks"), 0o700)
	_ = os.MkdirAll(filepath.Join(claudeDir, "chrome"), 0o700)
	_ = os.MkdirAll(filepath.Join(claudeDir, "session-env"), 0o700)
	_ = os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte("prompt"), 0o600)
	_ = os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte("{}"), 0o600)

	ws := t.TempDir()
	// Set up workspace-specific project memory
	var sb strings.Builder
	for _, r := range filepath.Clean(ws) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	memDir := filepath.Join(claudeDir, "projects", sb.String(), "memory")
	_ = os.MkdirAll(memDir, 0o700)

	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")

	// Base provider dir is bound
	if !strings.Contains(args, "--bind "+claudeDir+" "+claudeDir) {
		t.Errorf("expected base claude dir to be bound: %v", wrapped.Args)
	}

	// Protected subpaths overlayed read-only
	for _, sub := range []string{
		filepath.Join(claudeDir, "CLAUDE.md"),
		filepath.Join(claudeDir, "settings.json"),
		filepath.Join(claudeDir, "commands"),
		filepath.Join(claudeDir, "skills"),
		filepath.Join(claudeDir, "hooks"),
		filepath.Join(claudeDir, "chrome"),
		memDir,
	} {
		if !strings.Contains(args, "--ro-bind "+sub+" "+sub) {
			t.Errorf("expected protected subpath %s to be mounted --ro-bind, got args: %v", sub, wrapped.Args)
		}
	}

	// Ephemeral session dirs overlayed tmpfs
	sessionEnv := filepath.Join(claudeDir, "session-env")
	if !strings.Contains(args, "--tmpfs "+sessionEnv) {
		t.Errorf("expected ephemeral subpath %s to be mounted --tmpfs, got args: %v", sessionEnv, wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_CodexStateShielding(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap mount layout test")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	codexDir := filepath.Join(fakeHome, ".codex")
	_ = os.MkdirAll(filepath.Join(codexDir, "rules"), 0o700)
	_ = os.MkdirAll(filepath.Join(codexDir, "hooks"), 0o700)
	_ = os.MkdirAll(filepath.Join(codexDir, "shell_snapshots"), 0o700)
	_ = os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(""), 0o600)
	_ = os.WriteFile(filepath.Join(codexDir, "AGENTS.md"), []byte("agent"), 0o600)

	ws := t.TempDir()
	cmd := exec.Command("codex", "exec")
	cmd.Dir = ws

	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "codex", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}

	args := strings.Join(wrapped.Args, " ")

	if !strings.Contains(args, "--bind "+codexDir+" "+codexDir) {
		t.Errorf("expected base codex dir to be bound: %v", wrapped.Args)
	}

	for _, sub := range []string{
		filepath.Join(codexDir, "config.toml"),
		filepath.Join(codexDir, "AGENTS.md"),
		filepath.Join(codexDir, "rules"),
		filepath.Join(codexDir, "hooks"),
	} {
		if !strings.Contains(args, "--ro-bind "+sub+" "+sub) {
			t.Errorf("expected protected subpath %s to be mounted --ro-bind, got args: %v", sub, wrapped.Args)
		}
	}

	snapDir := filepath.Join(codexDir, "shell_snapshots")
	if !strings.Contains(args, "--tmpfs "+snapDir) {
		t.Errorf("expected ephemeral subpath %s to be mounted --tmpfs, got args: %v", snapDir, wrapped.Args)
	}
}

func TestWrapCLICommandWithSandbox_DisplayAndDBusSanitized(t *testing.T) {
	env := []string{
		"PATH=/bin:/usr/bin",
		"DISPLAY=:0",
		"XAUTHORITY=/tmp/fake.xauth",
		"WAYLAND_DISPLAY=wayland-0",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/dbus",
		"SAFE_VAR=hello",
	}
	sanitized := sanitizeCLIEnv("claude", env)
	joined := strings.Join(sanitized, " ")
	for _, forbidden := range []string{"DISPLAY=:0", "XAUTHORITY=", "WAYLAND_DISPLAY=", "DBUS_SESSION_BUS_ADDRESS="} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("expected %s to be sanitized, got: %v", forbidden, sanitized)
		}
	}
	if !strings.Contains(joined, "SAFE_VAR=hello") {
		t.Errorf("expected SAFE_VAR to be preserved, got: %v", sanitized)
	}
}

func TestWrapCLICommandWithSandbox_UnshareNetEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap mount layout test")
	}
	oldLookup := cliBwrapLookup
	defer func() { cliBwrapLookup = oldLookup }()
	cliBwrapLookup = func(file string) (string, error) {
		return "/usr/bin/bwrap", nil
	}

	ws := t.TempDir()
	cmd := exec.Command("claude", "-p", "test")
	cmd.Dir = ws

	// Without MAKEWAND_SANDBOX_UNSHARE_NET, --unshare-net is not present
	t.Setenv("MAKEWAND_SANDBOX_UNSHARE_NET", "")
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}
	if strings.Contains(strings.Join(wrapped.Args, " "), "--unshare-net") {
		t.Errorf("expected --unshare-net NOT to be present by default, got args: %v", wrapped.Args)
	}

	// With MAKEWAND_SANDBOX_UNSHARE_NET=1, --unshare-net is appended
	t.Setenv("MAKEWAND_SANDBOX_UNSHARE_NET", "1")
	wrapped2, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd)
	if err != nil {
		t.Fatalf("wrapCLICommandWithSandbox: %v", err)
	}
	if !strings.Contains(strings.Join(wrapped2.Args, " "), "--unshare-net") {
		t.Errorf("expected --unshare-net to be present when MAKEWAND_SANDBOX_UNSHARE_NET=1, got args: %v", wrapped2.Args)
	}
}
