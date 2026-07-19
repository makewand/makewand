package engine

import (
	"fmt"
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

func TestWriteFile_RejectsProtectedPaths(t *testing.T) {
	proj, err := NewProject("protected-paths", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "git hook", path: ".git/hooks/pre-commit", wantErr: true},
		{name: "nested git dir", path: "vendor/dep/.git/hooks/pre-commit", wantErr: true},
		{name: "github workflow", path: ".github/workflows/ci.yml", wantErr: true},
		{name: "circleci config", path: ".circleci/config.yml", wantErr: true},
		{name: "gitlab ci", path: ".gitlab-ci.yml", wantErr: true},
		{name: "jenkinsfile", path: "Jenkinsfile", wantErr: true},
		{name: "makefile", path: "Makefile", wantErr: true},
		{name: "test gate script", path: "scripts/test_gate.sh", wantErr: true},
		{name: "race gate script", path: "scripts/test_race.sh", wantErr: true},
		{name: "nested scripts shell", path: "scripts/ci/publish.sh", wantErr: true},
		{name: "regular source file", path: "main.go", wantErr: false},
		{name: "gitignore stays writable", path: ".gitignore", wantErr: false},
		{name: "nested source file", path: "pkg/util/util.go", wantErr: false},
		{name: "non-shell script stays writable", path: "scripts/gen.py", wantErr: false},
		{name: "app source under makefile name in subdir", path: "cmd/app/main.go", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := proj.WriteFile(tt.path, "content")
			if tt.wantErr && err == nil {
				t.Fatalf("WriteFile(%q) = nil, want protected path error", tt.path)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("WriteFile(%q) = %v, want nil", tt.path, err)
			}
		})
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

func TestScanFiles_ReturnsRootError(t *testing.T) {
	proj, err := NewProject("scan-root-error", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	if err := os.RemoveAll(proj.Path); err != nil {
		t.Fatalf("RemoveAll(project): %v", err)
	}

	if err := proj.ScanFiles(); err == nil {
		t.Fatal("ScanFiles should return an error when the project root is missing")
	}
}

func TestOpenProjectLimited_TruncatesLargeTree(t *testing.T) {
	projectDir := t.TempDir()
	for i := 0; i < 40; i++ {
		path := filepath.Join(projectDir, fmt.Sprintf("file-%02d.txt", i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	proj, err := OpenProjectLimited(projectDir, 10)
	if err != nil {
		t.Fatalf("OpenProjectLimited: %v", err)
	}
	if !proj.ScanTruncated {
		t.Fatal("expected ScanTruncated to be true")
	}
	if got := len(proj.Files); got > 11 {
		t.Fatalf("len(Files) = %d, want <= 11 (root + 10 entries)", got)
	}
}
