package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	if strings.Contains(args, claudePath) {
		t.Errorf("active provider credentials (%s) should NOT be masked: %v", claudePath, wrapped.Args)
	}
	if strings.Contains(args, claudeJSONPath) {
		t.Errorf("active provider credentials (%s) should NOT be masked: %v", claudeJSONPath, wrapped.Args)
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
