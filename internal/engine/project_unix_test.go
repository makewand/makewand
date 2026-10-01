//go:build !windows

package engine

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadFile_RejectsNonRegularFileFIFO(t *testing.T) {
	dir := t.TempDir()
	proj, err := OpenProject(dir)
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}

	fifoPath := filepath.Join(dir, "myfifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo not supported: %v", err)
	}

	if _, err := proj.ReadFile("myfifo"); err == nil {
		t.Fatal("ReadFile should error on FIFO, got nil")
	}
}
