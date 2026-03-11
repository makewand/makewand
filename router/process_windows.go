//go:build windows

package router

import "os/exec"

func setCLIProcessGroup(cmd *exec.Cmd) {
	_ = cmd
}

func killCLIProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
