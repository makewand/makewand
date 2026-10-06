//go:build windows

package backup

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// A restore requires the server to be stopped. A non-destructive, unshared
// read open conservatively refuses any live reader or writer of the database.
func stateDBInUse(path string) (bool, error) {
	name, err := restoreWindowsName(path)
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return false, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return false, fmt.Errorf("unsafe restore database object")
	}
	return false, nil
}
