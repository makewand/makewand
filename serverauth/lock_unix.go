//go:build !windows

package serverauth

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func lockHandle(f *os.File, exclusive bool) error {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	return syscall.Flock(int(f.Fd()), mode)
}

func unlockHandle(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func syncAuthDirectory(path string) error {
	// #nosec G304, G703 -- path is the directory containing the auth config
	//nolint:gosec // G304, G703
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// LockConfigFile acquires an advisory flock on <path>.lock.
// It returns an unlock function that must be called to release the lock and close the file.
func LockConfigFile(path string, exclusive bool) (func(), error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return func() {}, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	// #nosec G302, G304 -- lock file created with 0600 permissions alongside auth config
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockHandle(f, exclusive); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = unlockHandle(f)
		_ = f.Close()
	}, nil
}
