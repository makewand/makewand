//go:build windows

package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	applyNtQuerySecurityObject = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySecurityObject")
	applyNtSetSecurityObject   = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtSetSecurityObject")
)

// Query the saved descriptor, rather than GetSecurityInfo's presentation of
// legacy inheritance. NTFS bounds a descriptor at 64 KiB; one bounded query
// avoids a size/query race. Both NT APIs operate on the captured kernel object.
// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/nf-ntifs-ntquerysecurityobject
func applyQuerySecurity(handle windows.Handle, information windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
	if err := applyNtQuerySecurityObject.Find(); err != nil {
		return nil, err
	}
	data := make([]byte, 65536)
	var needed uint32
	status, _, _ := applyNtQuerySecurityObject.Call(uintptr(handle), uintptr(information),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&needed)))
	if status != 0 {
		return nil, fmt.Errorf("query captured object security: %w", windows.NTStatus(status&0xffffffff).Errno())
	}
	if needed < 20 || needed > 65536 {
		return nil, fmt.Errorf("captured security descriptor exceeds its bound")
	}
	sd := (*windows.SECURITY_DESCRIPTOR)(unsafe.Pointer(&data[0]))
	if !sd.IsValid() {
		return nil, fmt.Errorf("invalid captured security descriptor")
	}
	return sd, nil
}

// Application seals cover the entire actual DACL (including every ACE's
// order, SID, access mask and inheritance flags), plus its protected bit.
// Owner/group and inheritance bookkeeping on the descriptor are outside that
// contract. Construct a DACL-only descriptor without rewriting any ACEs.
func applySecurityValue(sd *windows.SECURITY_DESCRIPTOR) (string, error) {
	dacl, defaulted, err := sd.DACL()
	if err != nil {
		return "", err
	}
	control, _, err := sd.Control()
	if err != nil {
		return "", err
	}
	canonical, err := windows.NewSecurityDescriptor()
	if err != nil {
		return "", err
	}
	if err := canonical.SetDACL(dacl, true, defaulted); err != nil {
		return "", err
	}
	if err := canonical.SetControl(windows.SE_DACL_PROTECTED, control&windows.SE_DACL_PROTECTED); err != nil {
		return "", err
	}
	value := canonical.String()
	if value == "" {
		return "", fmt.Errorf("application DACL cannot be represented")
	}
	encoded, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		return "", err
	}
	encodedDACL, _, err := encoded.DACL()
	if err != nil {
		return "", err
	}
	// SDDL must preserve the actual ACE bytes. Unsupported ACEs fail closed
	// instead of silently weakening a journal's preimage or postimage seal.
	if err := applyEqualACEBytes(dacl, encodedDACL); err != nil {
		return "", err
	}
	runtime.KeepAlive(sd)
	runtime.KeepAlive(encoded)
	return value, nil
}

func applyEqualACEBytes(first, second *windows.ACL) error {
	if first == nil || second == nil {
		if first != second {
			return fmt.Errorf("application DACL changed its null state")
		}
		return nil
	}
	if first.AceCount != second.AceCount {
		return fmt.Errorf("application DACL changed its ACE count")
	}
	for i := uint32(0); i < uint32(first.AceCount); i++ {
		var a, b *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(first, i, &a); err != nil {
			return err
		}
		if err := windows.GetAce(second, i, &b); err != nil {
			return err
		}
		if a.Header.AceSize < 4 || b.Header.AceSize < 4 ||
			!bytes.Equal(unsafe.Slice((*byte)(unsafe.Pointer(a)), int(a.Header.AceSize)),
				unsafe.Slice((*byte)(unsafe.Pointer(b)), int(b.Header.AceSize))) {
			return fmt.Errorf("application DACL changed ACE %d", i)
		}
	}
	return nil
}

// NT's DACL information flag is 4. Protection is taken from the input
// descriptor; the Win32 PROTECTED/UNPROTECTED flags must not be passed here.
// Unlike SetSecurityInfo this does not recursively update existing children.
// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/nf-ntifs-ntsetsecurityobject
func applySetCapturedSecurity(handle windows.Handle, information windows.SECURITY_INFORMATION, sd *windows.SECURITY_DESCRIPTOR) error {
	if err := applyNtSetSecurityObject.Find(); err != nil {
		return err
	}
	status, _, _ := applyNtSetSecurityObject.Call(uintptr(handle), uintptr(information), uintptr(unsafe.Pointer(sd)))
	runtime.KeepAlive(sd)
	if status != 0 {
		return fmt.Errorf("set captured object security: %w", windows.NTStatus(status&0xffffffff).Errno())
	}
	return nil
}

func applySetCapturedDACL(handle windows.Handle, sd *windows.SECURITY_DESCRIPTOR) error {
	expected, err := applySecurityValue(sd)
	if err != nil {
		return err
	}
	if err := applySetCapturedSecurity(handle, windows.DACL_SECURITY_INFORMATION, sd); err != nil {
		return err
	}
	actual, err := applyQuerySecurity(handle, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	value, err := applySecurityValue(actual)
	if err != nil {
		return err
	}
	if value != expected {
		return fmt.Errorf("windows did not preserve the exact application DACL: %s, want %s", value, expected)
	}
	return nil
}

func privateApplyDirectory(path string) error {
	pins, err := pinApplyWindowsDirectories(path)
	if err != nil {
		return err
	}
	defer pins.close()
	// Obtain ownership and DACL rights on this pinned directory identity up
	// front. ReOpenFile on an administrative-token directory can refuse the
	// requested WRITE_OWNER even when CreateFile grants it on the fixed path.
	handle, err := openApplyWindowsHandle(path, true, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER|windows.FILE_READ_ATTRIBUTES)
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
	sd, err := applyQuerySecurity(handle, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return applySecurityValue(sd)
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
	sd, err := applyQuerySecurity(windows.Handle(handle), windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return applySecurityValue(sd)
}

func validateApplySecurity(value string) error {
	if value == "" {
		return fmt.Errorf("missing Windows apply file security")
	}
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		return err
	}
	_, err = applySecurityValue(sd)
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
	return applySetCapturedDACL(windows.Handle(handle), sd)
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
