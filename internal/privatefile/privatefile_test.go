package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTightenAndValidatePreserveCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content := []byte("complete existing private content")
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := Tighten(file); err != nil {
		t.Fatal(err)
	}
	if err := Validate(file); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(content) {
		t.Fatalf("privacy altered contents: %q, %v", got, err)
	}
}

func TestPrivateFileRejectsDirectoryAndClosedHandle(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if Tighten(file) == nil || Validate(file) == nil {
		t.Fatal("directory accepted as a private file")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if Tighten(file) == nil || Validate(file) == nil {
		t.Fatal("closed handle accepted")
	}
}
