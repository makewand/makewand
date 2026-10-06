//go:build unix

package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateDoesNotRepairBroadPermissions(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "private-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Chmod(0o644); err != nil {
		t.Fatal(err)
	}
	if Validate(file) == nil {
		t.Fatal("broad permissions accepted")
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("validation repaired an untrusted file: %v, %v", info, err)
	}
	if err := Tighten(file); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateFileRejectsHardLinksWithoutChangingPermissions(t *testing.T) {
	dir := t.TempDir()
	file, err := os.CreateTemp(dir, "private-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Chmod(0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file.Name(), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if Tighten(file) == nil || Validate(file) == nil {
		t.Fatal("multi-link file accepted")
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("rejected object was changed: %v, %v", info, err)
	}
}
