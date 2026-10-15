package router

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// readonlyFixContexts are the read-only sessions: reviews (read-only
// workspace) and remote-origin requests.
var readonlyFixContexts = []struct {
	name string
	ctx  context.Context
}{
	{"review", ContextWithTask(context.Background(), TaskReview)},
	{"remote origin", ContextWithRemoteOrigin(context.Background())},
}

// readonlyFixRunAndWait runs wrapped and waits until the private home holding
// src (a private copy bound into the sandbox) has been written back and
// removed. HOME is a tmpfs in read-only sessions, so the private home is found
// through src rather than through the HOME bind.
func readonlyFixRunAndWait(t *testing.T, wrapped *exec.Cmd, src string) string {
	t.Helper()
	done := make(chan string, 1)
	var once sync.Once
	hook := func(finished string) {
		if cliPathWithin(finished, src) {
			once.Do(func() { done <- finished })
		}
	}
	old := cliPrivateHomeFinished.Swap(&hook)
	t.Cleanup(func() { cliPrivateHomeFinished.Store(old) })
	out, err := runWrappedCLIForTest(wrapped)
	if err != nil {
		t.Fatalf("sandbox run failed: %v %s (args=%v)", err, out, wrapped.Args)
	}
	select {
	case dir := <-done:
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("private home %s was not removed: %v", dir, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("private home write-back did not run after the sandbox exited")
	}
	return string(out)
}

func readonlyFixWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Review and remote-origin sessions get writable private copies of the
// credential files, never the host files, and are watched for write-back like
// writable sessions; HOME stays an empty tmpfs and configuration a read-only
// bind of the host file.
func TestWritebackFix_ReadOnlySessionsUsePrivateCredentialCopies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	const claudeCred = `{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":1}}` //nolint:gosec // G101: fake fixture token
	const codexCred = `{"tokens":{"access_token":"a1","refresh_token":"r1"}}`                     //nolint:gosec // G101: fake fixture token
	for _, ro := range readonlyFixContexts {
		t.Run(ro.name, func(t *testing.T) {
			home, ws := canonicalTempDir(t), canonicalTempDir(t)
			selected := filepath.Join(home, ".codex-selected")
			for path, content := range map[string]string{
				filepath.Join(home, ".claude", ".credentials.json"): claudeCred,
				filepath.Join(home, ".claude", "settings.json"):     `{}`,
				filepath.Join(home, ".codex", "auth.json"):          codexCred,
				filepath.Join(home, ".codex", "config.toml"):        `model = "x"`,
				filepath.Join(selected, "auth.json"):                codexCred,
				filepath.Join(selected, "config.toml"):              `model = "x"`,
			} {
				readonlyFixWrite(t, path, content)
			}
			for _, run := range []struct {
				provider, dir, cred, config, content string
				env                                  []string
			}{
				{"claude", filepath.Join(home, ".claude"), ".credentials.json", "settings.json", claudeCred, nil},
				{"codex", filepath.Join(home, ".codex"), "auth.json", "config.toml", codexCred, nil},
				{"codex", selected, "auth.json", "config.toml", codexCred, []string{"CODEX_HOME=" + selected}},
			} {
				cmd := exec.Command("/usr/bin/true")
				cmd.Dir = ws
				cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + home}, run.env...)
				wrapped, err := wrapCLICommandWithSandbox(ro.ctx, run.provider, cmd)
				if err != nil {
					t.Fatalf("%s: %v", run.dir, err)
				}
				args := wrapped.Args
				if !cliHasMount(args, "--tmpfs", home) {
					t.Fatalf("%s: HOME must stay an empty tmpfs in read-only sessions: %v", run.dir, args)
				}
				for i := 0; i+2 < len(args); i++ {
					if args[i] == "--bind" && cliPathWithin(home, args[i+1]) {
						t.Fatalf("%s: host home path %s bound writable: %v", run.dir, args[i+1], args)
					}
				}
				priv := cliPrivateHomeSource(t, args, run.dir)
				if data, err := os.ReadFile(filepath.Join(priv, run.cred)); err != nil || string(data) != run.content {
					t.Fatalf("%s: credential not copied into the private copy: %q %v", run.dir, data, err)
				}
				config := filepath.Join(run.dir, run.config)
				if !auditFixHasTriple(args, "--ro-bind", config, config) {
					t.Fatalf("%s: configuration must stay a read-only bind of the host file: %v", run.dir, args)
				}
				if !slices.Contains(args, "--json-status-fd") {
					t.Fatalf("%s: read-only session not watched for write-back: %v", run.dir, args)
				}
			}
		})
	}
}

// End to end through a real bubblewrap sandbox: a token refresh made during a
// review or a remote-origin run (a provider that refreshes rotates its
// refresh token server-side) reaches the host through the same merge as in a
// writable run, refusals included; the review workspace and the provider
// configuration stay read-only.
func TestWritebackFix_LiveReadOnlySessionsWriteBackRefresh(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for _, ro := range readonlyFixContexts {
		for _, run := range []struct {
			provider, dir, rel, config, name string
			selected                         bool
		}{
			{"codex", ".codex", "auth.json", "config.toml", "codex refresh", false},
			{"codex", ".codex", "auth.json", "config.toml", "codex account swap", false},
			{"codex", "codex-account", "auth.json", "config.toml", "codex refresh", true},
			{"claude", ".claude", ".credentials.json", "settings.json", "claude refresh", false},
			{"agy", ".gemini", "antigravity-cli/antigravity-oauth-token", "settings.json",
				"agy token refresh with a rotated id_token (same sub/email)", false},
		} {
			t.Run(ro.name+": "+run.provider+" "+run.dir+": "+run.name, func(t *testing.T) {
				tc := writebackFixCase1(t, run.name)
				old, cur := writebackFixFile(t, tc.Host), writebackFixFile(t, tc.Sandbox)
				home, ws := canonicalTempDir(t), canonicalTempDir(t)
				dir := filepath.Join(home, run.dir)
				cred, config := filepath.Join(dir, filepath.FromSlash(run.rel)), filepath.Join(dir, run.config)
				readonlyFixWrite(t, cred, string(old))
				readonlyFixWrite(t, config, "{}\n")
				script := `printf '%s' "$1" > "$2.tmp" && mv -f "$2.tmp" "$2" && echo done
if printf x > "$3" 2>/dev/null; then echo CONFIG_WRITABLE; fi
if echo x > ws-probe 2>/dev/null; then echo WS_WRITABLE; fi`
				//nolint:gosec // G204: fixed fixture script; the credential JSON and paths are positional arguments, never shell source.
				cmd := exec.Command("/bin/sh", "-c", script, "fixture", string(cur), cred, config)
				cmd.Dir = ws
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
				if run.selected {
					cmd.Env = append(cmd.Env, "CODEX_HOME="+dir)
				}
				wrapped, err := wrapCLICommandWithSandbox(ro.ctx, run.provider, cmd)
				if err != nil {
					t.Fatal(err)
				}
				out := readonlyFixRunAndWait(t, wrapped, cliPrivateHomeSource(t, wrapped.Args, dir))
				if !strings.Contains(out, "done") || strings.Contains(out, "CONFIG_WRITABLE") {
					t.Fatalf("credential not writable or configuration writable inside the sandbox: %q", out)
				}
				if ro.name == "review" && strings.Contains(out, "WS_WRITABLE") {
					t.Fatalf("review workspace writable: %q", out)
				}
				if data, _ := os.ReadFile(config); string(data) != "{}\n" {
					t.Fatalf("configuration changed on the host: %q", data)
				}
				got, _ := os.ReadFile(cred)
				if tc.Expect.Refuse != nil {
					if !bytes.Equal(got, old) {
						t.Fatalf("host credential changed by a refused write-back: %q", got)
					}
					return
				}
				if want := writebackFixDecode(t, tc.Expect.Host); !reflect.DeepEqual(writebackFixDecode(t, got), want) {
					t.Fatalf("refresh not written back: host = %s, want %v", got, want)
				}
			})
		}
	}
}

// A failed host write is reported, like in the Python engine, and the host
// copy is kept.
func TestWritebackFix_HostWriteFailureIsReported(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX directory permissions as a non-root user")
	}
	old := `{"access_token":"a1","refresh_token":"r1"}`
	h, host, priv := writebackFixHome(t, "auth.json", &old)
	if err := os.WriteFile(priv, []byte(`{"access_token":"a2","refresh_token":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(host)
	if err := os.Chmod(dir, 0o500); err != nil { // no temporary file can be created next to it
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	warnings := h.writeBack()
	if data, _ := os.ReadFile(host); string(data) != old {
		t.Fatalf("host credential changed: %q", data)
	}
	if len(warnings) != 1 || !writebackFixNamesHost(warnings[0], host) || !strings.Contains(warnings[0], "failed to write back") {
		t.Fatalf("failed host write not reported: %q", warnings)
	}
}

// A login made inside the sandbox in a provider directory the host lacks
// (antigravity-cli/ below an existing ~/.gemini) is reported, like in the
// Python engine, and never written back.
func TestWritebackFix_LoginInMissingHostDirectoryIsReported(t *testing.T) {
	hostRoot := t.TempDir()
	h, err := newCLIPrivateHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)
	priv := filepath.Join(h.home, ".gemini")
	if err := os.MkdirAll(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	const rel = "antigravity-cli/antigravity-oauth-token"
	if err := h.copyIn(hostRoot, priv, cliPrivateCredFile{rel, true}); err != nil {
		t.Fatal(err)
	}
	readonlyFixWrite(t, filepath.Join(priv, filepath.FromSlash(rel)), `{"token":{"access_token":"ya29.A","refresh_token":"1//A"}}`)
	warnings := h.writeBack()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "log in on the host") ||
		!strings.Contains(warnings[0], filepath.Join(hostRoot, "antigravity-cli")) {
		t.Fatalf("login in a missing host directory not reported: %q", warnings)
	}
	if _, err := os.Stat(filepath.Join(hostRoot, "antigravity-cli")); !os.IsNotExist(err) {
		t.Fatalf("the login reached the host: %v", err)
	}
}

// The host file is checked again right before the rename, under the lock
// makewand's write-backs share (the sidecar ".<name>.makewand-lock"): a host
// refresh, replacement or touch that lands while the write-back waits for
// the lock is kept and reported instead of being overwritten.
func TestWritebackFix_HostChangedBeforeRenameIsKept(t *testing.T) {
	const old = `{"access_token":"a1","refresh_token":"r1"}`
	for _, change := range []struct {
		name  string
		apply func(t *testing.T, host string)
		want  string
	}{
		{"host refresh in place", func(t *testing.T, host string) {
			if err := os.WriteFile(host, []byte(`{"access_token":"host","refresh_token":"r1"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, `{"access_token":"host","refresh_token":"r1"}`},
		{"replaced by an identical file", func(t *testing.T, host string) {
			tmp := host + ".new"
			if err := os.WriteFile(tmp, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, host); err != nil {
				t.Fatal(err)
			}
		}, old},
		{"touched", func(t *testing.T, host string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(host, later, later); err != nil {
				t.Fatal(err)
			}
		}, old},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := old
			h, host, priv := writebackFixHome(t, "auth.json", &s)
			if err := os.WriteFile(priv, []byte(`{"access_token":"a2","refresh_token":"r1"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(filepath.Dir(host), ".auth.json.makewand-lock")
			lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Close() }()
			if err := lockUserHandle(lock, true); err != nil {
				t.Fatal(err)
			}
			result := make(chan []string, 1)
			go func() { result <- h.writeBack() }()
			// The synced temporary file next to the host file shows the
			// write-back has merged and now waits for the lock.
			deadline := time.After(10 * time.Second)
			for waiting := true; waiting; {
				select {
				case w := <-result:
					t.Fatalf("write-back finished while another write-back held the credential lock: %q", w)
				case <-deadline:
					t.Fatal("write-back never reached the credential lock")
				case <-time.After(10 * time.Millisecond):
					tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(host), ".auth.json.makewand-[0-9]*"))
					waiting = len(tmp) == 0
				}
			}
			change.apply(t, host)
			if err := unlockUserHandle(lock); err != nil {
				t.Fatal(err)
			}
			var warnings []string
			select {
			case warnings = <-result:
			case <-time.After(10 * time.Second):
				t.Fatal("write-back did not finish after the lock was released")
			}
			if data, _ := os.ReadFile(host); string(data) != change.want {
				t.Fatalf("host change overwritten: %q, want %q", data, change.want)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "changed during the run") {
				t.Fatalf("host change not reported: %q", warnings)
			}
		})
	}
}
