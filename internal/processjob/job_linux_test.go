//go:build linux

package processjob

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// equalsPathScript writes an executable script under a directory whose name
// contains '=', as Go subtest names put into temporary paths.
func equalsPathScript(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "seccomp=on")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	return path
}

// env(1) would read a target path containing '=' as an assignment; such a
// target reaches env through nice -n 0 and its arguments stay verbatim.
func TestWrapMemoryScopeNamesTargetWithEqualsUnambiguously(t *testing.T) {
	if _, _, ok := memoryScopeTools(); !ok {
		t.Skip("no reachable systemd user manager")
	}
	if scopeTools.nice == "" {
		t.Skip("no trusted nice")
	}
	target := equalsPathScript(t, "exit 0\n")
	cmd := exec.Command(target, "a=b", "--flag")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	wrapMemoryScope(cmd, 1<<30)
	joined := strings.Join(cmd.Args, " ")
	if !strings.HasSuffix(joined, " -- "+scopeTools.nice+" -n 0 "+target+" a=b --flag") {
		t.Fatalf("unexpected wrapper argv: %q", cmd.Args)
	}
	plain := exec.Command("/bin/sh", "-c", "x")
	plain.Env = []string{"PATH=/usr/bin:/bin"}
	wrapMemoryScope(plain, 1<<30)
	if strings.Contains(strings.Join(plain.Args, " "), " -n 0 ") {
		t.Fatalf("a target without '=' gained the trampoline: %q", plain.Args)
	}
}

// A command whose path contains '=' must run under the cap like any other,
// not exit 127 because env(1) took its path for an assignment.
func TestStartWithOptionsRunsTargetWithEqualsInPath(t *testing.T) {
	if MemoryEnforcement() != "systemd-scope" {
		t.Skip("memory cap not enforceable on this host (no systemd user manager)")
	}
	const limit = 512 << 20
	target := equalsPathScript(t, `cat /sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/memory.max
echo "args=$*"
echo "bus=${XDG_RUNTIME_DIR:-unset} invocation=${INVOCATION_ID:-unset}"
echo "nice=$(cut -d' ' -f19 /proc/self/stat)"
`)
	cmd := exec.Command(target, "x=1", "two words")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cleanup, err := StartWithOptions(cmd, Options{MemoryBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v; output %q", err, out.String())
	}
	self, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(self[bytes.LastIndexByte(self, ')')+1:]))
	got := out.String()
	for _, want := range []string{strconv.Itoa(limit), "args=x=1 two words", "bus=unset invocation=unset", "nice=" + fields[16]} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q lacks %q", got, want)
		}
	}
}

func TestWrapMemoryScopeRewritesCommandAndHidesBusEnv(t *testing.T) {
	if _, _, ok := memoryScopeTools(); !ok {
		t.Skip("no reachable systemd user manager")
	}
	cmd := exec.Command("/bin/sh", "-c", "x")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	wrapMemoryScope(cmd, 1<<30)
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "MemoryMax=1073741824") || !strings.Contains(joined, "MemorySwapMax=0") ||
		!strings.HasSuffix(joined, "-- /bin/sh -c x") || !strings.Contains(joined, "-u INVOCATION_ID") {
		t.Fatalf("unexpected wrapper argv: %q", cmd.Args)
	}
	if _, ok := os.LookupEnv("XDG_RUNTIME_DIR"); ok && !strings.Contains(joined, "-u XDG_RUNTIME_DIR") {
		t.Fatalf("bus variable added for systemd-run is not removed for the child: %q", cmd.Args)
	}
}

// The real child must observe the cgroup memory cap, keep its descriptors and
// not inherit the bus variables added only for systemd-run.
func TestStartWithOptionsAppliesLinuxMemoryCap(t *testing.T) {
	if MemoryEnforcement() != "systemd-scope" {
		t.Skip("memory cap not enforceable on this host (no systemd user manager)")
	}
	const limit = 512 << 20
	script := `cat /sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/memory.max; echo "bus=${XDG_RUNTIME_DIR:-unset}"; cat <&3`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("fd3-ok")
	w.Close()
	cmd.ExtraFiles = []*os.File{r}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cleanup, err := StartWithOptions(cmd, Options{MemoryBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	r.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v; output %q", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, strconv.Itoa(limit)) || !strings.Contains(got, "bus=unset") || !strings.Contains(got, "fd3-ok") {
		t.Fatalf("child did not run under the cap with clean env and fds: %q", got)
	}
}

func TestStartLeavesCommandUncappedWithoutMemoryBytes(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cleanup, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/bin/true" {
		t.Fatalf("Start without MemoryBytes rewrote the command: %q", cmd.Args)
	}
}

// A missing target must still fail synchronously in Start, not later as an
// exit status of the scope wrapper.
func TestStartWithMemoryCapKeepsMissingTargetStartError(t *testing.T) {
	cmd := exec.Command(t.TempDir() + "/missing-binary") //nolint:gosec // G204: deliberately missing test path
	cleanup, err := StartWithOptions(cmd, Options{MemoryBytes: 1 << 30})
	if err == nil {
		cleanup()
		_ = cmd.Wait()
		t.Fatalf("missing target started: %q", cmd.Args)
	}
}
