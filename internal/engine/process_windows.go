//go:build windows

package engine

import (
	"os/exec"
	"strconv"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Use taskkill /T /F to kill the entire process tree on Windows.
	_ = exec.Command("taskkill", "/T", "/F", "/PID",
		strconv.Itoa(cmd.Process.Pid)).Run()
}
