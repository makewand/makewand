//go:build windows

package privatefile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func replaceDACL(t *testing.T, file *os.File, value string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if value[:3] != "D:P" {
		flags = windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(file.Name(), windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// replaceInheritedDACL preserves an actual inherited ACE on the ordinary file.
// SetNamedSecurityInfo normalizes inheritance flags on non-container objects,
// so the fixture uses the NT setter and verifies the stored flag before testing
// Validate. This does not change the production permission policy.
func replaceInheritedDACL(t *testing.T, file *os.File, value string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		t.Fatal(err)
	}
	reopened, _, callErr := reopenFile.Call(uintptr(file.Fd()), uintptr(windows.READ_CONTROL|windows.WRITE_DAC), uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE), 0)
	if windows.Handle(reopened) == windows.InvalidHandle {
		t.Fatalf("open fixed test file for security: %v", callErr)
	}
	handle := windows.Handle(reopened)
	defer func() { _ = windows.CloseHandle(handle) }()
	setter := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtSetSecurityObject")
	status, _, _ := setter.Call(uintptr(handle), uintptr(windows.DACL_SECURITY_INFORMATION), uintptr(unsafe.Pointer(sd)))
	runtime.KeepAlive(sd)
	if int32(status) < 0 {
		t.Fatalf("set inherited test DACL: NTSTATUS 0x%08x", uint32(status))
	}
	stored, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := stored.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("test DACL protection was not preserved")
	}
	dacl, _, err := stored.DACL()
	if err != nil || dacl == nil {
		t.Fatal("stored test DACL unavailable")
	}
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	want := 2
	if user.IsWellKnown(windows.WinLocalSystemSid) {
		want = 1
	}
	if int(dacl.AceCount) != want {
		t.Fatal("stored test ACE count differs")
	}
	inheritedUser := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || uint32(ace.Mask) != privateFileAccess {
			t.Fatal("stored test ACE type or full-access mask differs")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			t.Fatal("stored test SID unavailable")
		}
		if sid.Equals(user) {
			if inheritedUser || ace.Header.AceFlags != windows.INHERITED_ACE {
				t.Fatal("actual inherited user ACE was not preserved")
			}
			inheritedUser = true
		} else if !sid.IsWellKnown(windows.WinLocalSystemSid) || ace.Header.AceFlags != 0 {
			t.Fatal("stored test SYSTEM ACE differs")
		}
	}
	if !inheritedUser {
		t.Fatal("actual inherited user ACE missing")
	}
}

func TestWindowsPrivateFileRequiresExactProtectedPermissionSet(t *testing.T) {
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	system := ""
	if !user.IsWellKnown(windows.WinLocalSystemSid) {
		system = "(A;;FA;;;SY)"
	}
	for name, value := range map[string]string{
		"extra_account":    "D:P(A;;FA;;;" + user.String() + ")" + system + "(A;;FR;;;WD)",
		"inherited_rights": "D:P(A;ID;FA;;;" + user.String() + ")" + system,
		"partial_rights":   "D:P(A;;FR;;;" + user.String() + ")" + system,
		"unprotected":      "D:(A;;FA;;;" + user.String() + ")" + system,
	} {
		t.Run(name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "private-*")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := Tighten(file); err != nil {
				t.Fatal(err)
			}
			if name == "inherited_rights" {
				replaceInheritedDACL(t, file, value)
			} else {
				replaceDACL(t, file, value)
			}
			if Validate(file) == nil {
				t.Fatal("non-exact DACL accepted")
			}
			// Validate is read-only, so a second validation must still reject.
			if Validate(file) == nil {
				t.Fatal("validation repaired untrusted permissions")
			}
			if err := Tighten(file); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsPrivateFileRejectsHardLinks(t *testing.T) {
	dir := t.TempDir()
	file, err := os.CreateTemp(dir, "private-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Link(file.Name(), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if Tighten(file) == nil || Validate(file) == nil {
		t.Fatal("multi-link file accepted")
	}
}
