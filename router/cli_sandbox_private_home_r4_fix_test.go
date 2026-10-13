package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// r4Sessions are the session kinds: reviews (read-only workspace),
// remote-origin requests and writable runs.
var r4Sessions = []struct {
	name     string
	ctx      context.Context
	readOnly bool
}{
	{"review", ContextWithTask(context.Background(), TaskReview), true},
	{"remote origin", ContextWithRemoteOrigin(context.Background()), true},
	{"writable", context.Background(), false},
}

// r4BindAt returns the source of the writable bind mounted at dest, or "".
func r4BindAt(args []string, dest string) string {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+2] == dest {
			return args[i+1]
		}
	}
	return ""
}

// r4NoHostPathBoundWritable fails if a path below the host home is the
// source of a writable bind.
func r4NoHostPathBoundWritable(t *testing.T, args []string, home string) {
	t.Helper()
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && cliPathWithin(home, args[i+1]) {
			t.Fatalf("host path %s bound writable: %v", args[i+1], args)
		}
	}
}

// r4Warnings collects the write-back warnings of every private home whose
// sandbox ran (cliPrivateHomeWarnings) until the test ends.
type r4Warnings struct {
	mu   sync.Mutex
	list []string
}

func r4CollectWarnings(t *testing.T) *r4Warnings {
	t.Helper()
	w := &r4Warnings{}
	hook := func(_ string, warnings []string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.list = append(w.list, warnings...)
	}
	old := cliPrivateHomeWarnings.Swap(&hook)
	t.Cleanup(func() { cliPrivateHomeWarnings.Store(old) })
	return w
}

func (w *r4Warnings) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.list
	w.list = nil
	return list
}

// r4StartSimulatedSandbox plays bubblewrap's part for a wrapped command that
// is never started: it reports the sandbox start on the --json-status-fd
// descriptor. Changes made to the private copies afterwards are what the
// sandboxed provider left behind; cliAfterWait then closes the descriptor
// like an exiting bwrap and waits for the write-back.
func r4StartSimulatedSandbox(t *testing.T, wrapped *exec.Cmd) {
	t.Helper()
	status := wrapped.ExtraFiles[len(wrapped.ExtraFiles)-1]
	if _, err := status.Write([]byte("{ \"child-pid\": 1 }\n")); err != nil {
		t.Fatal(err)
	}
}

// r4SimulateSandboxRun runs act as the sandboxed provider of wrapped and
// returns the warnings of the write-back that follows.
func r4SimulateSandboxRun(t *testing.T, wrapped *exec.Cmd, act func()) []string {
	t.Helper()
	warnings := r4CollectWarnings(t)
	r4StartSimulatedSandbox(t, wrapped)
	act()
	cliAfterWait(wrapped)
	return warnings.take()
}

// A credential copy that existed at launch and is deleted, replaced by a
// symlink, FIFO or directory, grown past the size limit or made unreadable
// inside the sandbox is reported once, naming the file: it may have held a
// rotated refresh token, so the host login may stop working. The host copy
// is kept.
func TestWritebackFix_R4_UnusablePrivateCopyIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	const old = `{"tokens":{"access_token":"a1","refresh_token":"r1"}}`
	const refresh = `{"tokens":{"access_token":"a2","refresh_token":"r2"}}` // a valid refresh behind a symlink
	elsewhere := t.TempDir()
	readonlyFixWrite(t, filepath.Join(elsewhere, "auth.json"), refresh)
	replace := func(t *testing.T, priv string, create func() error) {
		t.Helper()
		if err := os.Remove(priv); err != nil {
			t.Fatal(err)
		}
		if err := create(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, want string
		act        func(t *testing.T, priv string)
	}{
		{"deleted", "was deleted", func(t *testing.T, priv string) {
			if err := os.Remove(priv); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", "non-regular file", func(t *testing.T, priv string) {
			replace(t, priv, func() error { return os.Symlink(filepath.Join(elsewhere, "auth.json"), priv) })
		}},
		{"fifo", "non-regular file", func(t *testing.T, priv string) {
			replace(t, priv, func() error { return exec.Command("mkfifo", "-m", "600", priv).Run() })
		}},
		{"directory", "non-regular file", func(t *testing.T, priv string) {
			replace(t, priv, func() error { return os.Mkdir(priv, 0o700) })
		}},
		{"symlinked parent directory", "non-regular file", func(t *testing.T, priv string) {
			dir := filepath.Dir(priv)
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, dir); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized", "4 MiB", func(t *testing.T, priv string) {
			readonlyFixWrite(t, priv, `{"x":"`+strings.Repeat("a", cliPrivateCredMaxBytes)+`"}`)
		}},
		{"unreadable", "cannot be read", func(t *testing.T, priv string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file without permissions")
			}
			if err := os.Chmod(priv, 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := old
			h, host, priv := writebackFixHome(t, "auth.json", &s)
			tc.act(t, priv)
			warnings := h.writeBack()
			if got, _ := os.ReadFile(host); string(got) != old {
				t.Fatalf("host credential changed: %q", got)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], host) || !strings.Contains(warnings[0], tc.want) ||
				!strings.Contains(warnings[0], "log in again on the host") {
				t.Fatalf("an unusable private copy must be reported once, naming the file: %q", warnings)
			}
		})
	}
}

// A credential file the host lacked at launch is not reported while it stays
// absent; something unusable created in its place is.
func TestWritebackFix_R4_AbsentCredentialCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	h, host, priv := writebackFixHome(t, "auth.json", nil)
	if warnings := h.writeBack(); len(warnings) != 0 {
		t.Fatalf("a credential that stayed absent must not be reported: %q", warnings)
	}
	if err := os.Mkdir(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	if warnings := h.writeBack(); len(warnings) != 1 || !strings.Contains(warnings[0], host) ||
		!strings.Contains(warnings[0], "log in on the host") {
		t.Fatalf("an unusable credential created in the sandbox must be reported: %q", warnings)
	}
	if _, err := os.Lstat(host); !os.IsNotExist(err) {
		t.Fatalf("the host gained the credential: %v", err)
	}
}

// A provider directory the host lacks gets a private copy too, so a login
// made inside the sandbox can be reported: read-only sessions bind an empty
// private directory at its path inside the HOME tmpfs, writable sessions find
// it in the private HOME, and nothing is created on the host. Where bwrap
// could not create the mount point (HOME covered by another bind), nothing is
// bound there.
func TestWritebackFix_R4_AbsentProviderDirectoryMountPlan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	wrap := func(t *testing.T, ctx context.Context, home string) []string {
		t.Helper()
		cmd := exec.Command("/usr/bin/true")
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
		wrapped, err := wrapCLICommandWithSandbox(ctx, "agy", cmd)
		if err != nil {
			t.Fatal(err)
		}
		return wrapped.Args
	}
	for _, s := range r4Sessions {
		t.Run(s.name, func(t *testing.T) {
			home := t.TempDir()
			args := wrap(t, s.ctx, home)
			gemini := filepath.Join(home, ".gemini")
			r4NoHostPathBoundWritable(t, args, home)
			if s.readOnly {
				priv := cliPrivateHomeSource(t, args, gemini)
				if entries, err := os.ReadDir(priv); err != nil || len(entries) != 0 {
					t.Fatalf("the private copy of a missing provider directory must start empty: %v %v", entries, err)
				}
			} else {
				cliPrivateHomeSource(t, args, home)
				if src := r4BindAt(args, gemini); src != "" {
					t.Fatalf("writable sessions need no extra bind for a missing provider directory: %v", args)
				}
			}
			if _, err := os.Lstat(gemini); !os.IsNotExist(err) {
				t.Fatalf("missing provider directory created on the host: %v", err)
			}
		})
	}
	t.Run("mount point covered by a bind", func(t *testing.T) {
		home := t.TempDir()
		old := cliTestExtraBinds
		cliTestExtraBinds = func(string, []string) []string { return []string{"--ro-bind", home, home} }
		t.Cleanup(func() { cliTestExtraBinds = old })
		args := wrap(t, ContextWithTask(context.Background(), TaskReview), home)
		if src := r4BindAt(args, filepath.Join(home, ".gemini")); src != "" {
			t.Fatalf("mount point below a read-only bind must not be bound: %v", args)
		}
	})
}

func TestWritebackFix_R4_MountPointCreatable(t *testing.T) {
	const home = "/home/u"
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"HOME tmpfs", []string{"--ro-bind", "/", "/", "--tmpfs", "/home", "--tmpfs", home}, true},
		{"read-only root only", []string{"--ro-bind", "/", "/"}, false},
		{"HOME covered by a read-only bind", []string{"--tmpfs", home, "--ro-bind", home, home}, false},
		{"HOME covered by a host bind", []string{"--tmpfs", home, "--bind", "/srv/x", home}, false},
		{"HOME's parent bound later", []string{"--tmpfs", home, "--bind", "/srv/x", "/home"}, false},
		{"unrelated bind below HOME", []string{"--tmpfs", home, "--ro-bind", home + "/.local", home + "/.local"}, true},
		{"environment value naming a mount", []string{"--tmpfs", home, "--setenv", "--bind", home, "--chdir", "/"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cliMountPointCreatable(tc.args, home+"/.gemini"); got != tc.want {
				t.Fatalf("cliMountPointCreatable(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// End to end through a real bubblewrap sandbox: a login made inside the
// sandbox while the host has no provider directory at all is reported in
// review, remote-origin and writable sessions, and nothing reaches the host.
func TestWritebackFix_R4_LiveLoginWithoutHostDirectoryIsReported(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	const rel = ".gemini/antigravity-cli/antigravity-oauth-token"
	for _, s := range r4Sessions {
		t.Run(s.name, func(t *testing.T) {
			home, ws := t.TempDir(), t.TempDir()
			readonlyFixWrite(t, filepath.Join(home, ".profile"), "#\n")
			script := `mkdir -p "$HOME/.gemini/antigravity-cli" && printf '%s' "$1" > "$HOME/$2" && echo LOGGEDIN`
			//nolint:gosec // G204: fixed fixture script; the credential JSON and path are positional arguments, never shell source.
			cmd := exec.Command("/bin/sh", "-c", script, "fixture", `{"token":{"access_token":"ya29.N","refresh_token":"1//N"}}`, rel)
			cmd.Dir = ws
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
			warnings := r4CollectWarnings(t)
			wrapped, err := wrapCLICommandWithSandbox(s.ctx, "agy", cmd)
			if err != nil {
				t.Fatal(err)
			}
			out, err := runWrappedCLIForTest(wrapped)
			if err != nil || !strings.Contains(string(out), "LOGGEDIN") {
				t.Fatalf("sandbox run: %v %q", err, out)
			}
			host := filepath.Join(home, filepath.FromSlash(rel))
			got := warnings.take()
			if len(got) != 1 || !strings.Contains(got[0], host) || !strings.Contains(got[0], "log in on the host") {
				t.Fatalf("login without a host provider directory not reported: %q", got)
			}
			if _, err := os.Lstat(filepath.Join(home, ".gemini")); !os.IsNotExist(err) {
				t.Fatalf("the login reached the host: %v", err)
			}
		})
	}
}

// cliAfterWait returns only once the write-back of the run has finished: a
// write-back held up by another makewand write-back of the same file (the
// shared credential lock) keeps it waiting, and the merged token is on the
// host when it returns.
func TestWritebackFix_R4_AfterWaitWaitsForTheWriteBack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	home := t.TempDir()
	cred := filepath.Join(home, ".codex", "auth.json")
	const refreshed = `{"tokens":{"access_token":"a2","refresh_token":"r2"}}`
	readonlyFixWrite(t, cred, `{"tokens":{"access_token":"a1","refresh_token":"r1"}}`)
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "codex", cmd)
	if err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(cliPrivateHomeSource(t, wrapped.Args, home), ".codex", "auth.json")
	lock, err := os.OpenFile(cliCredentialLockPath(cred), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := lockUserHandle(lock, true); err != nil {
		t.Fatal(err)
	}
	r4StartSimulatedSandbox(t, wrapped)
	readonlyFixWrite(t, priv, refreshed)
	returned := make(chan struct{})
	go func() {
		cliAfterWait(wrapped)
		close(returned)
	}()
	// The synced temporary file next to the host file shows the write-back
	// has merged and now waits for the lock.
	deadline := time.After(10 * time.Second)
	for waiting := true; waiting; {
		select {
		case <-returned:
			t.Fatal("cliAfterWait returned before the write-back finished")
		case <-deadline:
			t.Fatal("write-back never reached the credential lock")
		case <-time.After(10 * time.Millisecond):
			tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(cred), ".auth.json.makewand-[0-9]*"))
			waiting = len(tmp) == 0
		}
	}
	select {
	case <-returned:
		t.Fatal("cliAfterWait returned while the write-back waited for the lock")
	case <-time.After(100 * time.Millisecond):
	}
	if err := unlockUserHandle(lock); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("cliAfterWait did not return after the write-back finished")
	}
	if data, _ := os.ReadFile(cred); string(data) != refreshed {
		t.Fatalf("refresh not on the host when cliAfterWait returned: %q", data)
	}
}

// End to end through a real bubblewrap sandbox with the production sequence
// (Start, cliAfterStart, Wait, cliAfterWait): a token refresh is on the host
// as soon as the run returns, and a run started right after copies in the
// refreshed tokens, never the rotated ones.
func TestWritebackFix_R4_LiveWriteBackCompleteWhenRunReturns(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	const old = `{"tokens":{"access_token":"a1","refresh_token":"r1","account_id":"acct"},"last_refresh":"2026-01-01T00:00:00Z"}`
	const fresh = `{"tokens":{"access_token":"a2","refresh_token":"r2","account_id":"acct"},"last_refresh":"2026-01-02T00:00:00Z"}`
	for _, s := range r4Sessions {
		t.Run(s.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				home, ws := t.TempDir(), t.TempDir()
				cred := filepath.Join(home, ".codex", "auth.json")
				readonlyFixWrite(t, cred, old)
				run := func(script string, args ...string) string {
					t.Helper()
					//nolint:gosec // G204: fixed fixture script; the credential JSON is a positional argument, never shell source.
					cmd := exec.Command("/bin/sh", append([]string{"-c", script, "fixture"}, args...)...)
					cmd.Dir = ws
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
					wrapped, err := wrapCLICommandWithSandbox(s.ctx, "codex", cmd)
					if err != nil {
						t.Fatal(err)
					}
					out, err := runWrappedCLIForTest(wrapped)
					if err != nil {
						t.Fatalf("sandbox run: %v %q", err, out)
					}
					return string(out)
				}
				run(`printf '%s' "$1" > "$HOME/.codex/auth.json"`, fresh)
				if data, _ := os.ReadFile(cred); string(data) != fresh {
					t.Fatalf("run %d: refresh not on the host when the run returned: %q", i, data)
				}
				if out := run(`cat "$HOME/.codex/auth.json"`); out != fresh {
					t.Fatalf("run %d: the next run copied in %q, want the refreshed tokens", i, out)
				}
			}
		})
	}
}
