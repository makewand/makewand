//go:build !windows

package quota

import (
	"os"
	"syscall"
)

func lockUserHandle(f *os.File, exclusive bool) error {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	return syscall.Flock(int(f.Fd()), mode)
}
func unlockUserHandle(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
