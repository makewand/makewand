package engine

import (
	"errors"
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

func TestReadFile_NormalAndEdgeCases(t *testing.T) {
	dir := t.TempDir()
	proj, err := OpenProject(dir)
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}

	// Write normal file
	if err := proj.WriteFile("hello.txt", "hello world"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	content, err := proj.ReadFile("hello.txt")
	if err != nil {
		t.Fatalf("ReadFile(hello.txt): %v", err)
	}
	if content != "hello world" {
		t.Fatalf("ReadFile content = %q, want %q", content, "hello world")
	}

	// Non-existent file
	if _, err := proj.ReadFile("nonexistent.txt"); err == nil {
		t.Fatal("ReadFile should error on nonexistent file, got nil")
	}

	// Subdir creation and reading a directory
	subDir := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := proj.ReadFile("subdir"); err == nil {
		t.Fatal("ReadFile should error on directory, got nil")
	}

	// Traversal
	if _, err := proj.ReadFile("../outside.txt"); err == nil {
		t.Fatal("ReadFile should error on path traversal, got nil")
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

func TestWriteFile_UnlinksPreExistingHardlink(t *testing.T) {
	proj, err := NewProject("hardlink-defense", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("original-external-content"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}

	target := filepath.Join(proj.Path, "linked.txt")
	if err := os.Link(outside, target); err != nil {
		t.Skipf("hard links not supported: %v", err)
	}

	if err := proj.WriteFile("linked.txt", "overwritten-content"); err != nil {
		t.Fatalf("WriteFile(linked.txt): %v", err)
	}

	outsideContent, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("ReadFile(outside): %v", err)
	}
	if string(outsideContent) != "original-external-content" {
		t.Fatalf("outside file was mutated through hardlink: got %q, want %q", string(outsideContent), "original-external-content")
	}

	projectContent, err := proj.ReadFile("linked.txt")
	if err != nil {
		t.Fatalf("ReadFile(linked.txt): %v", err)
	}
	if projectContent != "overwritten-content" {
		t.Fatalf("project file content = %q, want %q", projectContent, "overwritten-content")
	}
}

func TestProjectIgnoreDirs(t *testing.T) {
	for _, dir := range []string{".git", "node_modules", ".venv", "vendor", "target", "dist", "build", "__pycache__"} {
		if !IsProjectIgnoredDir(dir) {
			t.Errorf("IsProjectIgnoredDir(%q) = false, want true", dir)
		}
		if !shouldIgnore(dir) {
			t.Errorf("shouldIgnore(%q) = false, want true", dir)
		}
	}
	if IsProjectIgnoredDir("src") {
		t.Errorf("IsProjectIgnoredDir(\"src\") = true, want false")
	}
}

func TestNewProject_CollisionDefense(t *testing.T) {
	parent := t.TempDir()

	// 1. Success on new directory
	p1, err := NewProject("my-project", parent)
	if err != nil {
		t.Fatalf("NewProject(my-project): unexpected error: %v", err)
	}
	if p1.Name != "my-project" {
		t.Errorf("p1.Name = %q, want %q", p1.Name, "my-project")
	}

	// 2. Fails when directory already exists and contains files
	if err := os.WriteFile(filepath.Join(p1.Path, "important.txt"), []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err = NewProject("my-project", parent)
	if err == nil {
		t.Fatal("NewProject should fail on non-empty existing directory, got nil")
	}
	if !errors.Is(err, ErrProjectDirNotEmpty) {
		t.Errorf("NewProject error = %v, want ErrProjectDirNotEmpty", err)
	}

	// 3. Succeeds on an existing empty directory
	emptyDir := filepath.Join(parent, "empty-dir")
	if err := os.Mkdir(emptyDir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	pEmpty, err := NewProject("empty-dir", parent)
	if err != nil {
		t.Fatalf("NewProject on empty directory failed: %v", err)
	}
	if pEmpty.Path != emptyDir {
		t.Errorf("pEmpty.Path = %q, want %q", pEmpty.Path, emptyDir)
	}

	// 4. Fails when path is an existing regular file
	regularFile := filepath.Join(parent, "regular-file")
	if err := os.WriteFile(regularFile, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err = NewProject("regular-file", parent)
	if err == nil {
		t.Fatal("NewProject should fail when path exists and is not a directory, got nil")
	}
}

func TestProject_HasExistingFiles(t *testing.T) {
	parent := t.TempDir()
	p, err := NewProject("has-files", parent)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	if err := os.WriteFile(filepath.Join(p.Path, "exists.txt"), []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Check with non-existing file
	if p.HasExistingFiles([]ExtractedFile{{Path: "missing.txt"}}) {
		t.Errorf("HasExistingFiles(missing.txt) = true, want false")
	}

	// Check with existing file
	if !p.HasExistingFiles([]ExtractedFile{{Path: "exists.txt"}}) {
		t.Errorf("HasExistingFiles(exists.txt) = false, want true")
	}

	// Check with mixed files
	if !p.HasExistingFiles([]ExtractedFile{{Path: "missing.txt"}, {Path: "exists.txt"}}) {
		t.Errorf("HasExistingFiles(mixed) = false, want true")
	}
}

func TestSanitizeDirName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my project", "my-project"},
		{"My Project", "my-project"},
		{"../../evil", "evil"},
		{"", "project"},
		{"...---", "project"},
		{"cool:name*test?id", "cool-nametestid"},
	}

	for _, tt := range tests {
		got := SanitizeDirName(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeDirName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
