//go:build !windows

package execution

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func openLockFile(path string) (*os.File, error) {
	return openExecutionFile(path, syscall.O_CREAT|syscall.O_RDWR)
}

func openLedgerFile(path string) (*os.File, error) { return openExecutionFile(path, syscall.O_RDONLY) }
func openEventFile(path string) (*os.File, error) {
	return openExecutionFile(path, syscall.O_CREAT|syscall.O_APPEND|syscall.O_WRONLY)
}

func openExecutionFile(path string, flags int) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("execution accounting requires regular files")
	}
	return file, nil
}

func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func syncDirectory(path string) error {
	// #nosec G703 -- path is the caller-configured ledger directory, synced after atomic replacement.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
