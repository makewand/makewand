//go:build windows

package backup

import (
	"os"

	"golang.org/x/sys/windows"
)

// Open the leaf itself and deny modification/deletion until its security and
// complete bytes have been checked. Recovery closes it before changing files.
func openRestoreJournal(path string) (*os.File, error) {
	name, err := restoreWindowsName(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open restore journal", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
