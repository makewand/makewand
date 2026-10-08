//go:build windows

package serverauth

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
)

func lockHandle(f *os.File, exclusive bool) error {
	var flags uint32
	if exclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
}

func unlockHandle(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}

func syncAuthDirectory(string) error { return nil }

// LockConfigFile acquires an advisory file lock on <path>.lock.
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
