package backup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func stageAuthInstall(t *testing.T) (string, string, []restoreInstall) {
	t.Helper()
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte("old"), 0o640); err != nil { //nolint:gosec // G306: a non-0600 mode is what the test preserves
		t.Fatal(err)
	}
	if err := os.Chmod(auth, 0o640); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp, err := stageBeside(source, auth)
	if err != nil {
		t.Fatal(err)
	}
	return dir, auth, []restoreInstall{{Name: "auth.json", Tmp: tmp, Dst: auth}}
}

// A rollback whose "rolled_back" marker cannot be persisted must keep every
// preimage so a later recovery can still complete the rollback.
func TestRestoreRollbackKeepsPreimagesUntilRolledBackIsDurable(t *testing.T) {
	dir, auth, installs := stageAuthInstall(t)
	oldRename, oldPersist := renameFile, persistRestoreJournal
	t.Cleanup(func() { renameFile, persistRestoreJournal = oldRename, oldPersist })
	installFailure := errors.New("injected install failure")
	renameFile = func(string, string) error { return installFailure }
	markFailure := errors.New("injected rolled_back persistence failure")
	persistRestoreJournal = func(path string, j *restoreJournal) (bool, error) {
		if j.State == "rolled_back" {
			return false, markFailure
		}
		return oldPersist(path, j)
	}
	err := journaledInstall(Options{AuthConfigPath: auth}, installs)
	if !errors.Is(err, installFailure) || !errors.Is(err, markFailure) {
		t.Fatalf("want install and marker failures, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, restoreJournalName)); err != nil {
		t.Fatalf("prepared journal lost: %v", err)
	}
	renameFile, persistRestoreJournal = oldRename, oldPersist
	if err := RecoverRestore(Options{AuthConfigPath: auth}); err != nil {
		t.Fatalf("recovery could not finish rollback (preimage deleted early?): %v", err)
	}
	if readOrEmpty(auth) != "old" {
		t.Fatalf("auth=%q, want old", readOrEmpty(auth))
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(auth); err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("restored mode = %v, %v; want 0640", info.Mode().Perm(), err)
		}
	}
}

// Simulates a crash after cleanup deleted the images of a rolled-back journal:
// recovery must only finish cleanup, not attempt another rollback.
func TestRestoreRecoveryAcceptsRolledBackJournalWithoutImages(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := &restoreJournal{Version: 1, State: "rolled_back", Originals: []restoreOriginal{{
		Destination: auth, Backup: filepath.Join(dir, ".auth.json.restore-gone"), Hash: "deadbeef", Mode: 0o600, Existed: true,
	}}}
	if _, err := writeRestoreJournal(filepath.Join(dir, restoreJournalName), journal); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(Options{AuthConfigPath: auth}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, restoreJournalName)); !os.IsNotExist(err) {
		t.Fatalf("journal not cleaned: %v", err)
	}
	if readOrEmpty(auth) != "old" {
		t.Fatal("rolled-back recovery modified the live file")
	}
}

func TestStageBesideModeAppliesModeBeforeSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp, err := stageBesideMode(src, filepath.Join(dir, "dst"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(tmp); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("staged mode = %v, %v", info.Mode().Perm(), err)
	}
}
