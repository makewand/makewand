//go:build linux

package processjob

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

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
