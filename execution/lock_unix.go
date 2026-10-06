//go:build !windows

package execution

import (
	"errors"
	"os"
	"syscall"
)

func accountingOpenFlags(flags int) int { return flags | syscall.O_NONBLOCK | syscall.O_NOFOLLOW }

func accountingReparse(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }

func openLockFile(path string) (*os.File, error) {
	p, _, err := accountingPathForIdentity(path, nil)
	if err != nil {
		return nil, err
	}
	defer p.close()
	return openExecutionFile(p.root, p.name, os.O_CREATE|os.O_RDWR)
}

func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func syncDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
