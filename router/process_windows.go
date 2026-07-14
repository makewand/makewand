//go:build windows

package router

import (
	"os/exec"
	"strconv"
)

func setCLIProcessGroup(cmd *exec.Cmd) {
	_ = cmd
}

func killCLIProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Use taskkill /T /F to kill the entire process tree on Windows.
	// /T kills child processes, /F forces termination.
	pid := strconv.Itoa(cmd.Process.Pid)
	_ = exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()
}
