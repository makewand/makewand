//go:build windows

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPendingApprovalWindowsExistingDatabaseAndSidecarsBecomePrivate(t *testing.T) {
	p := pendingApprovalFixture(t)
	sealPendingApproval(t, p)
	path, _, err := pendingApprovalPath(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		file, err := os.OpenFile(filepath.Join(path, "approvals.sqlite"+suffix), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		err = applySetOpenFileSecurity(file, "D:P(A;;FA;;;WD)")
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := openPendingApprovalStore(context.Background(), p.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		security, err := applyFileSecurity(filepath.Join(path, "approvals.sqlite"+suffix))
		if err != nil || strings.Contains(security, ";;;WD)") || !strings.Contains(security, ";;;SY)") {
			t.Fatalf("existing state file is not private (%s): %s, %v", suffix, security, err)
		}
	}
}

func TestPendingApprovalWindowsSidecarHardlinkRefusesWithoutChangingOutsideACL(t *testing.T) {
	p := pendingApprovalFixture(t)
	sealPendingApproval(t, p)
	path, _, err := pendingApprovalPath(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := applyFileSecurity(outside)
	if err != nil {
		t.Fatal(err)
	}
	legacyBefore := legacyApplyWindowsSecurity(t, outside)
	if err := os.Link(outside, filepath.Join(path, "approvals.sqlite-wal")); err != nil {
		t.Fatal(err)
	}
	s, err := openPendingApprovalStore(context.Background(), p.Path, false)
	if s != nil {
		s.close()
	}
	if err == nil {
		t.Fatal("private SQLite state accepted a shared sidecar inode")
	}
	after, err := applyFileSecurity(outside)
	if err != nil || after != before {
		t.Fatalf("outside ACL changed: %s -> %s, %v", before, after, err)
	}
	if legacyAfter := legacyApplyWindowsSecurity(t, outside); legacyAfter != legacyBefore {
		t.Fatalf("outside legacy DACL changed: %s -> %s", legacyBefore, legacyAfter)
	}
}

func TestPendingApprovalWindowsPrivateDirectoryBindsCurrentUser(t *testing.T) {
	path := t.TempDir()
	if err := privateApplyDirectory(path); err != nil {
		t.Fatal(err)
	}
	handle, err := openApplyWindowsHandle(path, true, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatalf("default group owner was not bound to current user: %v", err)
	}
	actual, err := applyQuerySecurity(handle, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	actualValue, err := applySecurityValue(actual)
	if err != nil {
		t.Fatal(err)
	}
	expectedValue, err := applySecurityValue(expected)
	if err != nil {
		t.Fatal(err)
	}
	// The DACL contract preserves every ACE and its inheritance flags, plus
	// protection. Owner was checked independently above; AI is bookkeeping.
	if actualValue != expectedValue {
		t.Fatalf("private directory DACL differs: %s, want %s", actualValue, expectedValue)
	}
}
