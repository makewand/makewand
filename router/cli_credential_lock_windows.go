//go:build windows

package router

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// openCredentialLock opens, creating it, the sidecar lock file at path, but
// never through a reparse point (a symbolic link or junction) planted there:
// Windows has no O_NOFOLLOW, so a reparse point found by Lstat is refused
// before opening, and the file is opened as itself (FILE_FLAG_OPEN_REPARSE_POINT)
// and refused unless it is a plain file, should one appear in between.
func openCredentialLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && credentialLockIsReparsePoint(info) {
		return nil, fmt.Errorf("credential lock %s is a reparse point", path)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("credential lock %s is not a plain file", path)
	}
	return os.NewFile(uintptr(handle), path), nil
}

// credentialLockIsReparsePoint reports whether Lstat found a reparse point:
// a symbolic link, a mount point or any other reparse tag.
func credentialLockIsReparsePoint(info os.FileInfo) bool {
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
