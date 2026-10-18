//go:build !windows

package execution

import (
	"os/exec"
	"syscall"
)

func setLeaseFixtureGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
