//go:build linux

package backup

import "syscall"

// fOFDGetlk is F_OFD_GETLK. Open-file-description lock queries also report
// conflicting locks held by this same process (plain F_GETLK does not), so a
// connection opened in-process is detected too.
const fOFDGetlk = 36

func queryWriteLock(fd uintptr, lk *syscall.Flock_t) error {
	return syscall.FcntlFlock(fd, fOFDGetlk, lk)
}
