//go:build windows

package privatefile

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const privateFileAccess = 0x1f01ff // FILE_ALL_ACCESS, matching the FA SDDL right.

var reopenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

func validateObject(handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return fmt.Errorf("private file must be regular, non-reparse and single-link")
	}
	return nil
}

func currentUser() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func authorizedDefaultOwner(owner *windows.SID) error {
	token := windows.GetCurrentProcessToken()
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
		return fmt.Errorf("private file is owned by another account")
	}
	return nil
}

func fileOwner(handle windows.Handle) (*windows.SID, error) {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return nil, fmt.Errorf("private file owner unavailable")
	}
	return owner, nil
}

// Tighten normalizes an authorized default owner and installs a protected
// current-user/SYSTEM DACL through a handle to the same fixed file object.
func Tighten(file *os.File) error {
	handle := windows.Handle(file.Fd())
	if err := validateObject(handle); err != nil {
		return err
	}
	user, err := currentUser()
	if err != nil {
		return err
	}
	owner, err := fileOwner(handle)
	if err != nil {
		return err
	}
	access := uint32(windows.READ_CONTROL | windows.WRITE_DAC)
	securityInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var newOwner *windows.SID
	if !owner.Equals(user) {
		if err := authorizedDefaultOwner(owner); err != nil {
			return err
		}
		access |= windows.WRITE_OWNER
		securityInfo |= windows.OWNER_SECURITY_INFORMATION
		newOwner = user
	}
	reopened, _, callErr := reopenFile.Call(uintptr(handle), uintptr(access), uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE), 0)
	if windows.Handle(reopened) == windows.InvalidHandle {
		return fmt.Errorf("open fixed private file for security: %w", callErr)
	}
	privateHandle := windows.Handle(reopened)
	defer func() { _ = windows.CloseHandle(privateHandle) }()
	value := "D:P(A;;FA;;;" + user.String() + ")"
	if !user.IsWellKnown(windows.WinLocalSystemSid) {
		value += "(A;;FA;;;SY)"
	}
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("private file DACL unavailable")
	}
	if err := windows.SetSecurityInfo(privateHandle, windows.SE_FILE_OBJECT, securityInfo, newOwner, nil, dacl, nil); err != nil {
		return err
	}
	return Validate(file)
}

// Validate requires the complete protected, non-inherited FA permission set
// for the current user and SYSTEM, with no broad, duplicate or extra ACEs.
func Validate(file *os.File) error {
	handle := windows.Handle(file.Fd())
	if err := validateObject(handle); err != nil {
		return err
	}
	user, err := currentUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return fmt.Errorf("private file owner differs from the current user")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("private file DACL is not protected")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("private file DACL unavailable")
	}
	want := 2
	if user.IsWellKnown(windows.WinLocalSystemSid) {
		want = 1
	}
	if int(dacl.AceCount) != want {
		return fmt.Errorf("private file DACL contains unexpected accounts")
	}
	seen := map[string]bool{}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || uint32(ace.Mask) != privateFileAccess || ace.Header.AceSize < uint16(unsafe.Sizeof(windows.ACCESS_ALLOWED_ACE{})) {
			return fmt.Errorf("private file DACL contains unexpected permissions")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || (!sid.Equals(user) && !sid.IsWellKnown(windows.WinLocalSystemSid)) || seen[sid.String()] {
			return fmt.Errorf("private file DACL contains an unexpected or repeated account")
		}
		seen[sid.String()] = true
	}
	return nil
}
