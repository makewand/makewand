package router

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The credential lock is the sidecar file next to the host credential, the
// path the Python engine locks too (_credential_lock_path).
func TestWritebackFix_CredentialLockPathIsSidecar(t *testing.T) {
	host := filepath.FromSlash("/home/u/.codex/auth.json")
	if got, want := cliCredentialLockPath(host), filepath.FromSlash("/home/u/.codex/.auth.json.makewand-lock"); got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
}

// Another write-back that holds the credential lock longer than
// cliCredentialLockWait makes this one keep the host copy and report it,
// without leaving its temporary file behind.
func TestWritebackFix_CredentialLockTimeoutKeepsHost(t *testing.T) {
	wait := cliCredentialLockWait
	cliCredentialLockWait = 200 * time.Millisecond
	t.Cleanup(func() { cliCredentialLockWait = wait })
	old := `{"access_token":"a1","refresh_token":"r1"}`
	h, host, priv := writebackFixHome(t, "auth.json", &old)
	if err := os.WriteFile(priv, []byte(`{"access_token":"a2","refresh_token":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(cliCredentialLockPath(host), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := lockUserHandle(lock, true); err != nil {
		t.Fatal(err)
	}
	warnings := h.writeBack()
	if data, _ := os.ReadFile(host); string(data) != old {
		t.Fatalf("host credential written while another write-back held the lock: %q", data)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "another makewand process") {
		t.Fatalf("lock timeout not reported: %q", warnings)
	}
	if tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(host), ".auth.json.makewand-[0-9]*")); len(tmp) != 0 {
		t.Fatalf("temporary files left behind: %v", tmp)
	}
}

// The Python engine's credential lock (makewand/sandbox.py _credential_lock)
// and this engine's are one lock: while a Python write-back holds it, a Go
// write-back waits, then re-checks the host file and writes.
func TestWritebackFix_PythonEngineSharesCredentialLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Python engine locks with flock(2), which Windows lacks")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 unavailable")
	}
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	old := `{"access_token":"a1","refresh_token":"r1"}`
	h, host, priv := writebackFixHome(t, "auth.json", &old)
	if err := os.WriteFile(priv, []byte(`{"access_token":"a2","refresh_token":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `import sys
sys.path.insert(0, sys.argv[1])
from makewand import sandbox
with sandbox._credential_lock(sys.argv[2]) as locked:
    print("locked" if locked else "not locked", flush=True)
    sys.stdin.read()
`
	// #nosec G204 -- the local Python interpreter runs fixed test source; only test-owned paths are arguments.
	cmd := exec.Command(python, "-I", "-c", script, repo, host)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("Python engine did not take the credential lock: %q %v", line, err)
	}
	result := make(chan []string, 1)
	go func() { result <- h.writeBack() }()
	select {
	case w := <-result:
		t.Fatalf("Go write-back did not wait for the Python engine's lock: %q", w)
	case <-time.After(300 * time.Millisecond):
	}
	_ = stdin.Close() // the Python side leaves the with-block and releases the lock
	select {
	case w := <-result:
		if len(w) != 0 {
			t.Fatalf("unexpected warnings: %q", w)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Go write-back did not finish after the Python engine released the lock")
	}
	if data, _ := os.ReadFile(host); string(data) != `{"access_token":"a2","refresh_token":"r1"}` {
		t.Fatalf("refresh not written back after the lock was released: %q", data)
	}
}
