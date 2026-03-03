package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFile_RejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	outsideDir := filepath.Join(base, "outside")

	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(project): %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(outside): %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(projectDir, "link")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	proj, err := OpenProject(projectDir)
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}

	if err := proj.WriteFile("link/escaped.txt", "owned"); err == nil {
		t.Fatal("WriteFile should reject symlink escape, got nil error")
	}

	if _, err := os.Stat(filepath.Join(outsideDir, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file must not be created, stat err=%v", err)
	}
}

func TestReadFile_RejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	outsideDir := filepath.Join(base, "outside")

	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(project): %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(outside): %v", err)
	}

	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(projectDir, "secret-link.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	proj, err := OpenProject(projectDir)
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}

	if _, err := proj.ReadFile("secret-link.txt"); err == nil {
		t.Fatal("ReadFile should reject symlink escape, got nil error")
	}
}
