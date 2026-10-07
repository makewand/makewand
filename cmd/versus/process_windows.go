//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"github.com/makewand/makewand/internal/processjob"
)

func setProcessGroup(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	}
}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd != nil {
		_ = processjob.Kill(cmd)
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	_ = processjob.Kill(cmd)
}
