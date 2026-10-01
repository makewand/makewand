//go:build windows

package engine

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A token's authorized default owner may be the Administrators group. Accept
// only TokenUser or that exact TokenOwner, then explicitly normalize ownership
// through WRITE_OWNER on the same fixed object before protecting its DACL.
func applyPrivateOpenHandle(handle windows.Handle, directory bool) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("private state owner unavailable")
	}
	normalizeOwner := !owner.Equals(user.User.Sid)
	access := uint32(windows.READ_CONTROL | windows.WRITE_DAC)
	if normalizeOwner {
		var needed uint32
		_ = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &needed)
		if needed < uint32(unsafe.Sizeof(uintptr(0))) || needed > 65536 {
			return fmt.Errorf("token default owner unavailable")
		}
		data := make([]byte, needed)
		if err := windows.GetTokenInformation(token, windows.TokenOwner, &data[0], needed, &needed); err != nil {
			return err
		}
		defaultOwner := *(**windows.SID)(unsafe.Pointer(&data[0]))
		if defaultOwner == nil || !owner.Equals(defaultOwner) {
			return fmt.Errorf("private state is owned by another account")
		}
		access |= windows.WRITE_OWNER
	}
	privateHandle := handle
	if !directory {
		// A regular database handle from os.OpenFile needs additional security
		// rights. ReOpenFile keeps that same object; no pathname is resolved.
		reopened, _, callErr := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile").Call(
			uintptr(handle), uintptr(access), uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE), 0)
		if windows.Handle(reopened) == windows.InvalidHandle {
			return fmt.Errorf("open fixed state file for privacy: %w", callErr)
		}
		privateHandle = windows.Handle(reopened)
		defer func() { _ = windows.CloseHandle(privateHandle) }()
	}
	// Directory callers already acquired WRITE_OWNER/WRITE_DAC on their fixed,
	// non-reparse handle while every ancestor was pinned against replacement.
	if normalizeOwner {
		if err := windows.SetSecurityInfo(privateHandle, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
			return fmt.Errorf("normalize authorized default owner: %w", err)
		}
	}
	private, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return err
	}
	dacl, _, err := private.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(privateHandle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func privatePendingFile(file *os.File) error {
	return applyPrivateOpenHandle(windows.Handle(file.Fd()), false)
}
