//go:build unix && !linux

package backup

import "syscall"

func queryWriteLock(fd uintptr, lk *syscall.Flock_t) error {
	return syscall.FcntlFlock(fd, syscall.F_GETLK, lk)
}
