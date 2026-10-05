//go:build unix

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryRejectsJournalSymlinkWithoutRemovingItsTarget(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "journal.json")
	content := []byte(`{"version":1,"state":"committed"}`)
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, restoreJournalName)
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(Options{AuthConfigPath: filepath.Join(dir, "auth.json")}); err == nil {
		t.Fatal("recovery followed a journal symlink")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != string(content) {
		t.Fatalf("recovery changed the symlink target: %q, %v", got, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("recovery removed the untrusted journal link: %v, %v", info, err)
	}
}
