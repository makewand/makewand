//go:build windows

package router

import (
	"os/exec"
	"syscall"

	"github.com/makewand/makewand/internal/processjob"
)

func setCLIProcessGroup(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	}
}

func killCLIProcess(cmd *exec.Cmd) {
	_ = processjob.Kill(cmd)
}
