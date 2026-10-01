// Package processjob starts supervised subprocesses. Windows commands enter a
// non-breakaway Job before their primary thread runs, and descendants are
// terminated when their leader exits or supervision is canceled. This manages
// process lifetime; it is not a filesystem, credential, COM/WMI or network
// security sandbox.
package processjob

import (
	"fmt"
	"os/exec"
	"time"
)

// Options configure Windows Job limits. Zero leaves a resource unconstrained;
// callers using Start receive a default cap of 256 simultaneous processes.
// MemoryBytes limits total committed memory across the job, not disk usage.
type Options struct {
	MaxProcesses uint32
	MemoryBytes  uintptr
}

// Start creates the process and returns an idempotent cleanup function that
// must be called after Wait, or on any path abandoning supervision. Windows
// initialization fails closed: a child never runs outside its required job.
func Start(cmd *exec.Cmd) (func(), error) {
	return start(cmd, Options{MaxProcesses: 256})
}

func StartWithOptions(cmd *exec.Cmd, options Options) (func(), error) {
	return start(cmd, options)
}

func prepare(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process != nil {
		return fmt.Errorf("process supervision requires an unstarted command")
	}
	if cmd.WaitDelay == 0 {
		// Bound inherited pipes after leader exit even if an OS or an external
		// executable violates the normal descendant cleanup assumptions.
		cmd.WaitDelay = time.Second
	}
	return nil
}
