//go:build windows

package execution

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func openLockFile(path string) (*os.File, error) {
	return openExecutionFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
}

func openLedgerFile(path string) (*os.File, error) {
	return openExecutionFile(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
}
func openEventFile(path string) (*os.File, error) {
	return openExecutionFile(path, windows.FILE_APPEND_DATA, windows.OPEN_ALWAYS)
}

func openExecutionFile(path string, access, disposition uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	kind, kindErr := windows.GetFileType(handle)
	if kindErr != nil {
		_ = windows.CloseHandle(handle)
		return nil, kindErr
	}
	if kind != windows.FILE_TYPE_DISK || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("execution accounting requires regular files without reparse points")
	}
	return os.NewFile(uintptr(handle), path), nil
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
func syncDirectory(string) error { return nil }
