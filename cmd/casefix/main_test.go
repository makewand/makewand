package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/engine"
)

// TestWriteFiles_WritesContainedPaths confirms the dev tool writes normal
// project-relative files through the validated engine write path.
func TestWriteFiles_WritesContainedPaths(t *testing.T) {
	root := t.TempDir()

	written, err := writeFiles(root, []engine.ExtractedFile{
		{Path: "pkg/calc.go", Content: "package pkg\n"},
	})
	if err != nil {
		t.Fatalf("writeFiles: %v", err)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	got, err := os.ReadFile(filepath.Join(root, "pkg", "calc.go"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "package pkg\n" {
		t.Fatalf("content = %q, want %q", string(got), "package pkg\n")
	}
}

// TestWriteFiles_RejectsPathEscape ensures a `..` traversal is rejected and no
// file is written outside the project root — the same containment the product
// enforces now applies to the dev tool.
func TestWriteFiles_RejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(filepath.Dir(root), "escaped.go")
	_ = os.Remove(sibling)

	_, err := writeFiles(root, []engine.ExtractedFile{
		{Path: "../escaped.go", Content: "package escaped\n"},
	})
	if err == nil {
		t.Fatal("writeFiles(../escaped.go) = nil error, want containment rejection")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("error = %v, want a path traversal rejection", err)
	}
	if _, statErr := os.Stat(sibling); statErr == nil {
		t.Fatalf("escaped file was written outside the project root at %s", sibling)
	}
}

// TestWriteFiles_RejectsProtectedPath ensures protected CI/build files cannot be
// overwritten via the dev tool.
func TestWriteFiles_RejectsProtectedPath(t *testing.T) {
	root := t.TempDir()

	_, err := writeFiles(root, []engine.ExtractedFile{
		{Path: ".github/workflows/ci.yml", Content: "malicious\n"},
	})
	if err == nil {
		t.Fatal("writeFiles(.github/...) = nil error, want protected-path rejection")
	}
	if !strings.Contains(err.Error(), "protected") {
		t.Fatalf("error = %v, want a protected-path rejection", err)
	}
}
