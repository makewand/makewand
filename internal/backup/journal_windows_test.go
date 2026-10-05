//go:build windows

package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/privatefile"
	"golang.org/x/sys/windows"
)

func TestWindowsRestoreJournalIsPrivateAndRecoveryClosesReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, restoreJournalName)
	opts := Options{AuthConfigPath: filepath.Join(dir, "auth.json")}
	if published, err := writeRestoreJournal(path, &restoreJournal{Version: 1, State: "committed"}); !published || err != nil {
		t.Fatalf("publish: %v, %v", published, err)
	}
	file, err := openRestoreJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Validate(file); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(opts); err != nil {
		t.Fatalf("private journal recovery/cleanup: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal reader blocked cleanup: %v", err)
	}
}

func TestWindowsRecoveryRejectsBroadJournalWithoutRepairingOrChangingLiveFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(live, []byte("unchanged live state"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, restoreJournalName)
	if _, err := writeRestoreJournal(path, &restoreJournal{Version: 1, State: "committed"}); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	value := "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		value += "(A;;FA;;;SY)"
	}
	value += "(A;;FR;;;WD)"
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(Options{AuthConfigPath: live}); err == nil || !strings.Contains(err.Error(), "unsafe restore journal permissions") {
		t.Fatalf("broad journal accepted: %v", err)
	}
	if got := readOrEmpty(live); got != "unchanged live state" {
		t.Fatalf("unsafe recovery changed live state: %q", got)
	}
	file, err := openRestoreJournal(path)
	if err != nil {
		t.Fatalf("unsafe journal was removed: %v", err)
	}
	defer file.Close()
	if privatefile.Validate(file) == nil {
		t.Fatal("recovery repaired untrusted permissions")
	}
}

func TestWindowsStateDBInUseDetectsRealOpenHandleWithoutTouchingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if busy, err := stateDBInUse(path); err != nil || busy {
		t.Fatalf("missing file considered busy: %v, %v", busy, err)
	}
	if err := os.WriteFile(path, []byte("original complete database image"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := stateDBInUse(path); err != nil || !busy {
		file.Close()
		t.Fatalf("real reader not detected: %v, %v", busy, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if busy, err := stateDBInUse(path); err != nil || busy {
		t.Fatalf("closed file considered busy: %v, %v", busy, err)
	}
	if got := readOrEmpty(path); got != "original complete database image" {
		t.Fatalf("in-use probe changed bytes: %q", got)
	}
	if _, err := Restore(makeArchive(t), Options{StateDBPath: path}); errors.Is(err, ErrStateDBInUse) {
		t.Fatalf("closed file still blocks a restore: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
}

func TestWindowsDeepJournalAndBusyProbePreserveLogicalPaths(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 310 {
		dir = filepath.Join(dir, strings.Repeat("long-local-component", 3))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(live, []byte("unchanged live image"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := stateDBInUse(live); err != nil || !busy {
		file.Close()
		t.Fatalf("deep open file not detected: %v, %v", busy, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if busy, err := stateDBInUse(live); err != nil || busy {
		t.Fatalf("deep closed file not usable: %v, %v", busy, err)
	}
	path := filepath.Join(dir, restoreJournalName)
	if _, err := writeRestoreJournal(path, &restoreJournal{Version: 1, State: "committed"}); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(Options{AuthConfigPath: live}); err != nil {
		t.Fatalf("deep journal recovery: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deep journal not cleaned: %v", err)
	}
	if got := readOrEmpty(live); got != "unchanged live image" {
		t.Fatalf("deep recovery changed live image: %q", got)
	}
}
