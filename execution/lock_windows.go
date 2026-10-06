//go:build windows

package execution

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"syscall"
)

func accountingOpenFlags(flags int) int { return flags | windows.FILE_FLAG_OPEN_REPARSE_POINT }

func accountingReparse(info os.FileInfo) bool {
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return info.Mode()&os.ModeSymlink != 0 || !ok || attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}
func unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
func syncDirectory(*os.Root) error { return nil }
