package engine

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/makewand/makewand/internal/seccompbpf"
)

// sandboxSeccompFD is the child descriptor carrying the BPF program. Engine
// bwrap launches pass no other ExtraFiles, so the filter is always fd 3.
const sandboxSeccompFD = 3

// Test seams.
var (
	sandboxSeccompGOOS      = runtime.GOOS
	sandboxSeccompSupported = seccompbpf.Supported
	sandboxSeccompGetenv    = os.Getenv
	sandboxSeccompWarn      = func(msg string) { fmt.Fprintln(os.Stderr, msg) }
	sandboxSeccompWarnOnce  = &sync.Once{}
)

// sandboxSeccompArgs returns the leading bwrap arguments that install the
// syscall filter. On linux/amd64 and linux/arm64 the filter is required;
// MAKEWAND_ALLOW_NO_SECCOMP=1 opts out with a warning. Architectures without a
// filter run unfiltered with a warning. The arguments are always placed first
// in the bwrap argv so launchers can recognise them (sandboxSeccompRequested).
func sandboxSeccompArgs() []string {
	if sandboxSeccompGOOS != "linux" {
		return nil
	}
	if strings.TrimSpace(sandboxSeccompGetenv("MAKEWAND_ALLOW_NO_SECCOMP")) == "1" {
		sandboxSeccompWarnOnce.Do(func() {
			sandboxSeccompWarn("makewand: warning: MAKEWAND_ALLOW_NO_SECCOMP=1, sandboxed commands run without the seccomp syscall filter")
		})
		return nil
	}
	if !sandboxSeccompSupported() {
		sandboxSeccompWarnOnce.Do(func() {
			sandboxSeccompWarn("makewand: warning: no seccomp filter for " + runtime.GOARCH + "; sandboxed commands run without a syscall filter")
		})
		return nil
	}
	return []string{"--seccomp", strconv.Itoa(sandboxSeccompFD)}
}

// sandboxSeccompRequested reports whether a wrapped bwrap argv expects the
// filter descriptor.
func sandboxSeccompRequested(args []string) bool {
	return len(args) >= 2 && args[0] == "--seccomp" && args[1] == strconv.Itoa(sandboxSeccompFD)
}

// attachSandboxSeccomp supplies the filter as fd 3 when args request it. The
// returned function closes the parent's copy and must run after Start (it is
// safe to call on every path). A launch that requests the filter but cannot
// supply it fails closed; bwrap would also reject a missing descriptor.
func attachSandboxSeccomp(cmd *exec.Cmd, args []string) (func(), error) {
	if !sandboxSeccompRequested(args) {
		return func() {}, nil
	}
	if len(cmd.ExtraFiles) != sandboxSeccompFD-3 {
		return nil, fmt.Errorf("seccomp filter must be child fd %d", sandboxSeccompFD)
	}
	pipe, err := seccompbpf.NewFilterPipe()
	if err != nil {
		return nil, fmt.Errorf("sandbox seccomp filter: %w (set MAKEWAND_ALLOW_NO_SECCOMP=1 to run without it, unsafe)", err)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, pipe)
	var once sync.Once
	return func() { once.Do(func() { _ = pipe.Close() }) }, nil
}
