//go:build windows

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func windowsSecurityValueFixture(t *testing.T, value string) string {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		t.Fatal(err)
	}
	result, err := applySecurityValue(sd)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestApplyWindowsSecurityCanonicalizationPreservesEveryACEAndProtection(t *testing.T) {
	base := "D:(A;ID;FA;;;SY)(A;ID;FR;;;BA)"
	want := windowsSecurityValueFixture(t, base)
	// These descriptor fields do not change the DACL's actual ACEs or its
	// protection. GetSecurityInfo can add them while presenting legacy ACLs.
	for _, value := range []string{
		"O:BAG:BAD:(A;ID;FA;;;SY)(A;ID;FR;;;BA)",
		"D:AI(A;ID;FA;;;SY)(A;ID;FR;;;BA)",
	} {
		if got := windowsSecurityValueFixture(t, value); got != want {
			t.Fatalf("irrelevant descriptor metadata altered the DACL seal: %s, want %s", got, want)
		}
	}
	for _, tc := range []struct{ name, value string }{
		{"inheritance", "D:(A;;FA;;;SY)(A;ID;FR;;;BA)"},
		{"child_inheritance", "D:(A;OICIID;FA;;;SY)(A;ID;FR;;;BA)"},
		{"order", "D:(A;ID;FR;;;BA)(A;ID;FA;;;SY)"},
		{"mask", "D:(A;ID;FR;;;SY)(A;ID;FR;;;BA)"},
		{"sid", "D:(A;ID;FA;;;WD)(A;ID;FR;;;BA)"},
		{"type", "D:(D;ID;FA;;;SY)(A;ID;FR;;;BA)"},
		{"protected", "D:P(A;ID;FA;;;SY)(A;ID;FR;;;BA)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowsSecurityValueFixture(t, tc.value); got == want {
				t.Fatalf("real DACL change disappeared from seal: %s", tc.value)
			}
		})
	}
}

// Retain the previous Win32 view as independent round-trip evidence. A raw NT
// seal alone must not conceal an externally visible inherited ACE mutation.
func legacyApplyWindowsSecurity(t *testing.T, path string) string {
	t.Helper()
	pins, err := pinApplyWindowsDirectories(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer pins.close()
	handle, err := openApplyWindowsHandle(path, false, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	value, err := applySecurityValue(sd)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestApplyWindowsInheritedDACLExactCommitAndRecovery(t *testing.T) {
	p := applyFixture(t)
	if err := p.WriteFile("inherited.txt", "before"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.Path, "inherited.txt")
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	expected := "D:(A;ID;FA;;;" + user.User.Sid.String() + ")"
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		expected += "(A;ID;FA;;;SY)"
	}
	expected += "(A;ID;FR;;;WD)"
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = applySetOpenFileSecurity(file, expected)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := applyFileSecurity(path)
	if err != nil || sealed != windowsSecurityValueFixture(t, expected) {
		t.Fatalf("inherited DACL was rewritten before publication: %s, %v", sealed, err)
	}
	legacy := legacyApplyWindowsSecurity(t, path)
	t.Logf("inherited DACL raw=%s legacy=%s", sealed, legacy)
	assertSecurity := func() {
		t.Helper()
		actual, err := applyFileSecurity(path)
		if err != nil || actual != sealed {
			t.Fatalf("raw inherited DACL changed: %s -> %s, %v", sealed, actual, err)
		}
		if actual := legacyApplyWindowsSecurity(t, path); actual != legacy {
			t.Fatalf("legacy inherited DACL changed: %s -> %s", legacy, actual)
		}
	}
	if err := p.ApplyFilesTransactional(context.Background(), []ExtractedFile{{Path: "inherited.txt", Content: "committed"}}, nil); err != nil {
		t.Fatal(err)
	}
	assertSecurity()
	requireApplyContent(t, p, "inherited.txt", "committed")
	state, err := p.openApplyState(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	journal, err := p.prepareApplyJournal(context.Background(), state,
		[]ExtractedFile{{Path: "inherited.txt", Content: "interrupted"}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := openBoundApplyRoot(p.Path, journal.RootID)
	if err != nil {
		t.Fatal(err)
	}
	err = writeApplyEntry(root, state, journal, 0, "interrupted")
	_ = root.Close()
	if err != nil {
		t.Fatal(err)
	}
	assertSecurity()
	if err := p.recoverApplyLocked(state); err != nil {
		t.Fatal(err)
	}
	assertSecurity()
	requireApplyContent(t, p, "inherited.txt", "committed")
}

func TestApplyWindowsRecoveryFinalPreimageCheckPreservesLateACLEdit(t *testing.T) {
	p := applyFixture(t)
	state, _ := preparePartialApply(t, p)
	defer state.close()
	var edited string
	state.afterRestore = func(entry applyJournalEntry) error {
		if entry.Path == filepath.Join("nested", "new.txt") {
			edited = setWindowsUserOnlyACL(t, filepath.Join(p.Path, "a.txt"))
		}
		return nil
	}
	if err := p.recoverApplyLocked(state); !errors.Is(err, ErrStaleApply) {
		t.Fatalf("recovery published success after a restored ACL changed: %v", err)
	}
	requireApplyContent(t, p, "a.txt", "old-a")
	requireApplyContent(t, p, "b.txt", "old-b")
	actual, err := applyFileSecurity(filepath.Join(p.Path, "a.txt"))
	if err != nil || edited == "" || actual != edited {
		t.Fatalf("late external ACL edit lost: %s -> %s, %v", edited, actual, err)
	}
	journal, err := state.read()
	if err != nil || journal.State != "applying" {
		t.Fatalf("recovery evidence was finalized: %v, %v", journal, err)
	}
}
