//go:build windows

package backup

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func acquireRestoreLock(path string) (*os.File, error) {
	name, err := restoreWindowsName(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("unsafe restore lock")
	}
	file := os.NewFile(uintptr(handle), path)
	err = windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrRestoreInProgress
		}
		return nil, err
	}
	return file, nil
}

// Windows offers no POSIX directory fsync; file data and journal headers are
// flushed. Recovery here covers process interruption rather than power loss.
func syncDirectory(string) error { return nil }
