//go:build windows

package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func privateApplyDirectory(path string) error {
	pins, err := pinApplyWindowsDirectories(path)
	if err != nil {
		return err
	}
	defer pins.close()
	handle, err := openApplyWindowsHandle(path, true, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return applyPrivateOpenHandle(handle, true)
}

func openApplyLock(path string) (*os.File, error) {
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
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("apply lock must be an unlinked regular file")
	}
	return os.NewFile(uintptr(handle), path), nil
}

func tryApplyLock(file *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockApply(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}

// Windows has no POSIX directory fsync. File data and journal headers are
// flushed; this contract covers process interruption, not storage power loss.
func syncApplyDirectory(string) error { return nil }

func applyFileSecurity(path string) (string, error) {
	pins, err := pinApplyWindowsDirectories(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer pins.close()
	handle, err := openApplyWindowsHandle(path, false, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}

func applyOpenFileSecurity(file *os.File) (string, error) {
	// ReOpenFile references the same kernel object. GENERIC_WRITE on the
	// original temporary handle does not imply READ_CONTROL, and resolving its
	// name again would allow a rename/reparse race during ACL sealing.
	handle, _, callErr := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile").Call(
		file.Fd(), uintptr(windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE), 0)
	if windows.Handle(handle) == windows.InvalidHandle {
		return "", fmt.Errorf("reopen fixed file for ACL capture: %w", callErr)
	}
	defer func() { _ = windows.CloseHandle(windows.Handle(handle)) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(handle), &info); err != nil {
		return "", err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return "", fmt.Errorf("apply image must hold a non-reparse regular file")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(handle), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}

func validateApplySecurity(value string) error {
	if value == "" {
		return fmt.Errorf("missing Windows apply file security")
	}
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		return err
	}
	_, _, err = sd.DACL()
	return err
}

func applySetOpenFileSecurity(file *os.File, value string) error {
	if value == "" {
		return nil
	}
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	// os.OpenFile's GENERIC_WRITE does not include WRITE_DAC. Reopen the same
	// kernel file object with the required rights, rather than resolving its
	// pathname again after creation or introducing a rename/reparse window.
	handle, _, callErr := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile").Call(
		file.Fd(), uintptr(windows.WRITE_DAC|windows.READ_CONTROL),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE), 0)
	if windows.Handle(handle) == windows.InvalidHandle {
		return fmt.Errorf("reopen fixed file for ACL preservation: %w", callErr)
	}
	defer func() { _ = windows.CloseHandle(windows.Handle(handle)) }()
	return windows.SetSecurityInfo(windows.Handle(handle), windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil)
}

func supportedApplyMode(mode os.FileMode) os.FileMode {
	if mode&0o200 == 0 {
		return 0o444
	}
	return 0o666
}

func applyWorkspaceIdentity(path string) (string, error) {
	pins, err := pinApplyWindowsDirectories(path)
	if err != nil {
		return "", err
	}
	defer pins.close()
	handle := pins.handles[len(pins.handles)-1]
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", fmt.Errorf("apply workspace must be a real directory")
	}
	return fmt.Sprintf("%x:%x:%x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func applyOpenRootIdentity(root *os.Root) (string, error) {
	file, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", fmt.Errorf("apply root must hold a non-reparse directory")
	}
	return fmt.Sprintf("%x:%x:%x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}
