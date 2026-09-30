package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointIdentityRejectsNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointIdentity(dir, info); err == nil {
		t.Fatal("directory received a regular file identity")
	}
}

func TestCheckpointIdentityTracksHardlinksAndReplacement(t *testing.T) {
	dir := t.TempDir()
	first, sibling := filepath.Join(dir, "first"), filepath.Join(dir, "sibling")
	if err := os.WriteFile(first, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, sibling); err != nil {
		t.Skipf("hardlink creation is unavailable: %v", err)
	}
	identify := func(path string) checkpointFileIdentity {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := checkpointIdentity(path, info)
		if err != nil {
			t.Fatal(err)
		}
		return identity
	}
	before, linked := identify(first), identify(sibling)
	if before.Nlink < 2 || !before.sameFile(linked) {
		t.Fatal("hardlinked files did not share a reliable identity")
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if identify(first).sameFile(linked) {
		t.Fatal("atomic replacement retained the shared identity")
	}
}
