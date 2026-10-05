//go:build unix

package remotesession

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func openStoreLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	euid := os.Geteuid()
	if euid < 0 {
		file.Close()
		return nil, fmt.Errorf("session lock owner is unavailable")
	}
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || int64(stat.Uid) != int64(euid) {
		file.Close()
		return nil, fmt.Errorf("session lock must be a private regular file")
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return nil, err
		}
	}
	return file, nil
}

func lockStoreHandle(file *os.File, exclusive bool) error {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	return syscall.Flock(int(file.Fd()), mode)
}

func tryExclusiveStoreLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func syncStoreDirectory(path string) error {
	// #nosec G703 -- path is the operator-configured store directory, synced after atomic session publication or cleanup.
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
