//go:build !windows

package processjob

import (
	"os"
	"os/exec"
)

func start(cmd *exec.Cmd, _ Options) (func(), error) {
	if err := prepare(cmd); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Unix callers retain their existing process-group or sandbox policies.
	return func() {}, nil
}

// Kill exists for shared call sites. Unix process-group cancellation remains
// in the platform helpers, because a plain process is not a process group.
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
