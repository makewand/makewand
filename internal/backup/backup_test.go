package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewBackup(t *testing.T) {
	b := NewBackup()
	if b.manifest == nil {
		t.Fatal("manifest should not be nil")
	}
	if b.manifest.SchemaVersion != "makewand.backup.v1" {
		t.Errorf("schema version = %q, want makewand.backup.v1", b.manifest.SchemaVersion)
	}
}

func TestAddFile(t *testing.T) {
	tmpdir := t.TempDir()
	testFile := filepath.Join(tmpdir, "test.db")
	if err := os.WriteFile(testFile, []byte("test data"), 0o600); err != nil {
		t.Fatalf("create test file: %v", err)
	}

	b := NewBackup()
	info, _ := os.Stat(testFile)
	f := b.AddFile("test.db", info)

	if f.Name != "test.db" {
		t.Errorf("name = %q, want test.db", f.Name)
	}
	if f.Size != 9 {
		t.Errorf("size = %d, want 9", f.Size)
	}
}

func TestComputeFileHash(t *testing.T) {
	tmpdir := t.TempDir()
	testFile := filepath.Join(tmpdir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o600); err != nil {
		t.Fatalf("create test file: %v", err)
	}

	hash, err := ComputeFileHash(testFile)
	if err != nil {
		t.Fatalf("ComputeFileHash: %v", err)
	}

	if hash == "" {
		t.Fatal("hash should not be empty")
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 (SHA-256)", len(hash))
	}
}

func TestVerifyFile(t *testing.T) {
	tmpdir := t.TempDir()
	testFile := filepath.Join(tmpdir, "test.txt")
	content := []byte("test content")
	if err := os.WriteFile(testFile, content, 0o600); err != nil {
		t.Fatalf("create test file: %v", err)
	}

	hash, _ := ComputeFileHash(testFile)

	// Should pass with correct hash
	if err := VerifyFile(testFile, hash); err != nil {
		t.Fatalf("VerifyFile with correct hash: %v", err)
	}

	// Should fail with incorrect hash
	if err := VerifyFile(testFile, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("VerifyFile should fail with incorrect hash")
	}
}

func TestManifestSerialization(t *testing.T) {
	tmpdir := t.TempDir()
	manifestPath := filepath.Join(tmpdir, "manifest.json")

	// Create and write manifest
	b := NewBackup()
	tmpFile := filepath.Join(tmpdir, "data.db")
	if err := os.WriteFile(tmpFile, []byte("data"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	info, _ := os.Stat(tmpFile)
	b.AddFile("data.db", info)
	hash, _ := ComputeFileHash(tmpFile)
	if err := b.SetFileHash("data.db", hash); err != nil {
		t.Fatalf("SetFileHash: %v", err)
	}
	if err := b.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if err := b.WriteManifest(manifestPath); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	// Load and verify
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	if m.SchemaVersion != "makewand.backup.v1" {
		t.Errorf("schema version = %q", m.SchemaVersion)
	}
	if len(m.Files) != 1 {
		t.Errorf("files count = %d, want 1", len(m.Files))
	}
	if m.Files[0].Name != "data.db" {
		t.Errorf("first file name = %q, want data.db", m.Files[0].Name)
	}
}
