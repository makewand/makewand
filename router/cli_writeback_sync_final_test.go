//go:build linux

package router

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Final round: every production path that runs a sandboxed provider CLI
// (Chat, ChatStream, the availability probe) returns only once the
// credential write-back of the run's private provider home has landed, and
// that wait is bounded.

const (
	syncFinalOld = `{"tokens":{"access_token":"a1","refresh_token":"r1","account_id":"acct"},"last_refresh":"2026-01-01T00:00:00Z"}`
	syncFinalNew = `{"tokens":{"access_token":"a2","refresh_token":"r2","account_id":"acct"},"last_refresh":"2026-01-02T00:00:00Z"}`
	// syncFinalRefresh is a fake provider CLI that refreshes its codex token
	// and answers; syncFinalShow prints the token its private home holds.
	syncFinalRefresh = `printf '%s' '` + syncFinalNew + `' > "$HOME/.codex/auth.json"; echo answer`
	syncFinalShow    = `cat "$HOME/.codex/auth.json"`
)

// syncFinalSessions are the session kinds of a provider run: a review
// (read-only workspace), a writable run in a workspace, and a remote-origin
// request (scratch workspace).
var syncFinalSessions = []struct {
	name string
	ctx  func(ws string) context.Context
}{
	{"review", func(ws string) context.Context {
		return ContextWithWorkDir(ContextWithTask(context.Background(), TaskReview), ws)
	}},
	{"writable", func(ws string) context.Context { return ContextWithWorkDir(context.Background(), ws) }},
	{"remote origin", func(string) context.Context { return ContextWithRemoteOrigin(context.Background()) }},
}

// syncFinalHome gives the test a host HOME holding a codex token and returns
// the token file.
func syncFinalHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cred := filepath.Join(home, ".codex", "auth.json")
	readonlyFixWrite(t, cred, syncFinalOld)
	return cred
}

func syncFinalRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Chat returns only once the token its provider refreshed in the sandbox is
// on the host, so a process exiting right after Chat keeps it and a Chat
// started next copies in the refreshed tokens, never the rotated ones.
func TestWritebackSyncFinal_ChatReturnsAfterTheWriteBack(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for _, s := range syncFinalSessions {
		t.Run(s.name, func(t *testing.T) {
			for i := 0; i < 5; i++ {
				cred := syncFinalHome(t)
				ws := t.TempDir()
				refresh := NewCommandCLI("codex", "/bin/sh", []string{"-c", syncFinalRefresh}, PromptModeArg)
				show := NewCommandCLI("codex", "/bin/sh", []string{"-c", syncFinalShow}, PromptModeArg)
				out, _, err := refresh.Chat(s.ctx(ws), []Message{{Role: "user", Content: "hi"}}, "", 100)
				if err != nil || strings.TrimSpace(out) != "answer" {
					t.Fatalf("run %d: Chat = %q, %v", i, out, err)
				}
				if got := syncFinalRead(t, cred); got != syncFinalNew {
					t.Fatalf("run %d: refreshed token not on the host when Chat returned: %q", i, got)
				}
				out, _, err = show.Chat(s.ctx(ws), []Message{{Role: "user", Content: "hi"}}, "", 100)
				if err != nil || strings.TrimSpace(out) != syncFinalNew {
					t.Fatalf("run %d: the next Chat copied in %q (%v), want the refreshed token", i, out, err)
				}
			}
		})
	}
}

// A stream ends (its terminal chunk arrives) only once the token its provider
// refreshed in the sandbox is on the host.
func TestWritebackSyncFinal_StreamEndsAfterTheWriteBack(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for _, s := range syncFinalSessions {
		t.Run(s.name, func(t *testing.T) {
			for i := 0; i < 5; i++ {
				cred := syncFinalHome(t)
				refresh := NewCommandCLI("codex", "/bin/sh", []string{"-c", syncFinalRefresh}, PromptModeArg)
				stream, err := refresh.ChatStream(s.ctx(t.TempDir()), []Message{{Role: "user", Content: "hi"}}, "", 100)
				if err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				done := false
				for chunk := range stream {
					if chunk.Error != nil {
						t.Fatalf("run %d: stream error: %v", i, chunk.Error)
					}
					text.WriteString(chunk.Content)
					if chunk.Done {
						done = true
						if got := syncFinalRead(t, cred); got != syncFinalNew {
							t.Fatalf("run %d: refreshed token not on the host when the stream ended: %q", i, got)
						}
					}
				}
				if !done || strings.TrimSpace(text.String()) != "answer" {
					t.Fatalf("run %d: stream = %q, done = %v", i, text.String(), done)
				}
			}
		})
	}
}

// The availability probe waits for the write-back of a probe command that
// runs in a sandbox with a private provider home.
func TestWritebackSyncFinal_HealthProbeReturnsAfterTheWriteBack(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for i := 0; i < 5; i++ {
		cred := syncFinalHome(t)
		ws := t.TempDir()
		var wrapErr error
		p := &CLIProvider{name: "codex-custom-cli", binPath: "/bin/sh", provider: "codex"}
		p.checkCmd = func(ctx context.Context) *exec.Cmd {
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", syncFinalRefresh)
			cmd.Dir = ws
			wrapped, err := wrapCLICommandWithSandbox(ContextWithTask(ctx, TaskReview), "codex", cmd)
			if err != nil {
				wrapErr = err
				return nil
			}
			return wrapped
		}
		if !p.healthCheck() || wrapErr != nil {
			t.Fatalf("run %d: probe failed: %v", i, wrapErr)
		}
		if got := syncFinalRead(t, cred); got != syncFinalNew {
			t.Fatalf("run %d: refreshed token not on the host when the probe returned: %q", i, got)
		}
	}
}

// syncFinalStderr captures what is written to os.Stderr until the returned
// function is called.
func syncFinalStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(copied) }()
	var once sync.Once
	stop := func() string {
		once.Do(func() { os.Stderr = old; _ = w.Close(); <-copied })
		return buf.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

// syncFinalShortWait shortens the bound of cliAfterWait for the test.
func syncFinalShortWait(t *testing.T, lockWait, slack time.Duration) {
	t.Helper()
	oldLock, oldSlack := cliCredentialLockWait, cliWriteBackSlack
	cliCredentialLockWait, cliWriteBackSlack = lockWait, slack
	t.Cleanup(func() { cliCredentialLockWait, cliWriteBackSlack = oldLock, oldSlack })
}

// syncFinalWrapped returns a wrapped (never started) codex command with a
// private home whose host token file is cred, and the private copy of it.
func syncFinalWrapped(t *testing.T) (wrapped *exec.Cmd, cred, priv string) {
	t.Helper()
	home := t.TempDir()
	cred = filepath.Join(home, ".codex", "auth.json")
	readonlyFixWrite(t, cred, syncFinalOld)
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "codex", cmd)
	if err != nil {
		t.Fatal(err)
	}
	priv = filepath.Join(cliPrivateHomeSource(t, wrapped.Args, home), ".codex", "auth.json")
	return wrapped, cred, priv
}

// A sandbox process that keeps the status descriptor open (one that never
// exits) cannot hang the run: cliAfterWait gives up after its bound and says
// so, and the write-back still lands once the sandbox is gone.
func TestWritebackSyncFinal_AfterWaitIsBounded(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	syncFinalShortWait(t, 100*time.Millisecond, 300*time.Millisecond)
	wrapped, cred, priv := syncFinalWrapped(t)
	dir := filepath.Dir(filepath.Dir(filepath.Dir(priv)))
	finished := make(chan struct{}, 1)
	hook := func(d string) {
		if d == dir {
			select {
			case finished <- struct{}{}:
			default:
			}
		}
	}
	old := cliPrivateHomeFinished.Swap(&hook)
	t.Cleanup(func() { cliPrivateHomeFinished.Store(old) })
	status := wrapped.ExtraFiles[len(wrapped.ExtraFiles)-1]
	// The "sandbox" starts, refreshes the token and keeps running: it holds a
	// copy of the status descriptor.
	held, err := syscall.Dup(int(status.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	stillRunning := os.NewFile(uintptr(held), "held status descriptor")
	t.Cleanup(func() { _ = stillRunning.Close() })
	r4StartSimulatedSandbox(t, wrapped)
	readonlyFixWrite(t, priv, syncFinalNew)

	stderr := syncFinalStderr(t)
	returned := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		cliAfterWait(wrapped)
		returned <- time.Since(start)
	}()
	select {
	case took := <-returned:
		if took < cliWriteBackWait() {
			t.Fatalf("cliAfterWait returned after %s, before its bound %s, while the sandbox still ran", took, cliWriteBackWait())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cliAfterWait hung on a sandbox that never exits")
	}
	warning := stderr()
	if !strings.Contains(warning, "makewand: WARNING: the credential write-back of sandbox home "+dir+" has not finished") {
		t.Fatalf("stderr = %q, want a warning naming the private home", warning)
	}
	if got := syncFinalRead(t, cred); got != syncFinalOld {
		t.Fatalf("host token changed while the sandbox still ran: %q", got)
	}
	// Once the sandbox is gone the write-back lands in the background.
	_ = stillRunning.Close()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("write-back never finished after the sandbox exited")
	}
	if got := syncFinalRead(t, cred); got != syncFinalNew {
		t.Fatalf("refresh not written back after the sandbox exited: %q", got)
	}
	// A second call returns at once.
	start = time.Now()
	cliAfterWait(wrapped)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("second cliAfterWait took %s", took)
	}
}

// A write-back that waits for the credential lock of another makewand
// write-back is waited for: the bound covers the lock wait, and the run
// returns with the write-back's own verdict (here: lock wait timed out)
// before the bound, so without giving up on it. (The write-back prints its
// warning from its own goroutine, so os.Stderr is not swapped here.)
func TestWritebackSyncFinal_AfterWaitCoversTheCredentialLockWait(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	syncFinalShortWait(t, 300*time.Millisecond, 2*time.Second)
	warnings := r4CollectWarnings(t)
	wrapped, cred, priv := syncFinalWrapped(t)
	lock, err := os.OpenFile(cliCredentialLockPath(cred), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := lockUserHandle(lock, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unlockUserHandle(lock) })
	r4StartSimulatedSandbox(t, wrapped)
	readonlyFixWrite(t, priv, syncFinalNew)
	start := time.Now()
	cliAfterWait(wrapped)
	took := time.Since(start)
	if took < cliCredentialLockWait {
		t.Fatalf("cliAfterWait returned after %s, before the lock wait %s ended", took, cliCredentialLockWait)
	}
	if took >= cliWriteBackWait() {
		t.Fatalf("cliAfterWait gave up after %s on a write-back that ends with its lock wait", took)
	}
	got := warnings.take()
	if len(got) != 1 || !strings.Contains(got[0], "lock wait timed out") {
		t.Fatalf("write-back warnings = %q, want the lock wait reported before the run returned", got)
	}
}

// syncFinalRunDirs lists the private homes left in base.
func syncFinalRunDirs(t *testing.T, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		dirs = append(dirs, e.Name())
	}
	return dirs
}

// A provider command that cannot be started never ran, so nothing is written
// back, but its private home - copies of the account's tokens - is removed
// before Chat or ChatStream returns, not whenever the garbage collector gets
// to the command (or never, when the process exits first).
func TestWritebackSyncFinal_FailedStartRemovesThePrivateHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	base := t.TempDir()
	oldBase := cliPrivateHomeBaseDir
	cliPrivateHomeBaseDir = func() string { return base }
	t.Cleanup(func() { cliPrivateHomeBaseDir = oldBase })
	syncFinalHome(t)
	// An invalid memory cap makes the sandboxed start fail before anything runs.
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "not-a-size")
	provider := NewCommandCLI("codex", "/bin/sh", []string{"-c", syncFinalShow}, PromptModeArg)
	ctx := ContextWithWorkDir(context.Background(), t.TempDir())
	if _, _, err := provider.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, "", 100); err == nil {
		t.Fatal("Chat succeeded with an invalid memory cap")
	}
	if left := syncFinalRunDirs(t, base); len(left) != 0 {
		t.Fatalf("Chat left private homes behind after a failed start: %v", left)
	}
	if _, err := provider.ChatStream(ctx, []Message{{Role: "user", Content: "hi"}}, "", 100); err == nil {
		t.Fatal("ChatStream succeeded with an invalid memory cap")
	}
	if left := syncFinalRunDirs(t, base); len(left) != 0 {
		t.Fatalf("ChatStream left private homes behind after a failed start: %v", left)
	}
}
