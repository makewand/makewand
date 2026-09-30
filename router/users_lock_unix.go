//go:build !windows

package router

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
func syncUserDirectory(path string) error {
	// #nosec G703 -- path is the operator-configured user data directory, passed by atomic replacement for durability sync.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
