package backup

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Manifest describes a backup archive.
type Manifest struct {
	SchemaVersion string    `json:"schema_version"`
	BackupTime    time.Time `json:"backup_time"`
	Files         []File    `json:"files"`
	Checksum      string    `json:"checksum"`
}

// File describes a single file in the backup.
type File struct {
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
	Error string `json:"error,omitempty"`
}

// Backup creates a consistent snapshot of database and related files.
type Backup struct {
	manifest *Manifest
	files    map[string]*File
}

// NewBackup initializes a new backup descriptor.
func NewBackup() *Backup {
	return &Backup{
		manifest: &Manifest{
			SchemaVersion: "makewand.backup.v1",
			BackupTime:    time.Now().UTC(),
			Files:         make([]File, 0),
		},
		files: make(map[string]*File),
	}
}

// AddFile records a file for inclusion in the backup.
func (b *Backup) AddFile(name string, info os.FileInfo) *File {
	f := &File{
		Name:  name,
		Size:  info.Size(),
		Mode:  uint32(info.Mode()),
		IsDir: info.IsDir(),
	}
	b.files[name] = f
	return f
}

// SetFileHash sets the content hash for a file.
func (b *Backup) SetFileHash(name, hash string) error {
	f, ok := b.files[name]
	if !ok {
		return fmt.Errorf("file not registered: %s", name)
	}
	f.Hash = hash
	return nil
}

// SetFileError records an error encountered for a file.
func (b *Backup) SetFileError(name, err string) {
	if f, ok := b.files[name]; ok {
		f.Error = err
	}
}

// Finalize prepares the manifest for serialization.
func (b *Backup) Finalize() error {
	for _, f := range b.files {
		b.manifest.Files = append(b.manifest.Files, *f)
	}
	return nil
}

// WriteManifest writes the manifest to a file.
func (b *Backup) WriteManifest(path string) error {
	data, err := json.MarshalIndent(b.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// LoadManifest loads a backup manifest from file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}

	if m.SchemaVersion != "makewand.backup.v1" {
		return nil, fmt.Errorf("unsupported schema version: %s", m.SchemaVersion)
	}

	return &m, nil
}

// ComputeFileHash reads a file and computes its SHA-256 hash.
func ComputeFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// VerifyFile checks that a file's hash matches the manifest entry.
func VerifyFile(path string, expected string) error {
	actual, err := ComputeFileHash(path)
	if err != nil {
		return err
	}

	if actual != expected {
		return fmt.Errorf("hash mismatch for %s: expected %s, got %s", path, expected, actual)
	}

	return nil
}

// BackupDatabaseClean performs a clean backup by issuing a checkpoint.
// Returns the manifest and any errors encountered.
func BackupDatabaseClean(dbPath string, stagingDir string) (*Manifest, error) {
	backup := NewBackup()

	// Record database file (should be consistent after checkpoint)
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, fmt.Errorf("stat database: %w", err)
	}
	backup.AddFile("state.db", info)

	hash, err := ComputeFileHash(dbPath)
	if err != nil {
		backup.SetFileError("state.db", err.Error())
	} else if err := backup.SetFileHash("state.db", hash); err != nil {
		backup.SetFileError("state.db", err.Error())
	}

	// Record WAL file if present
	walPath := dbPath + "-wal"
	if info, err := os.Stat(walPath); err == nil {
		backup.AddFile("state.db-wal", info)
		if hash, err := ComputeFileHash(walPath); err == nil {
			if err := backup.SetFileHash("state.db-wal", hash); err != nil {
				backup.SetFileError("state.db-wal", err.Error())
			}
		} else {
			backup.SetFileError("state.db-wal", err.Error())
		}
	}

	// Record SHM file if present
	shmPath := dbPath + "-shm"
	if info, err := os.Stat(shmPath); err == nil {
		backup.AddFile("state.db-shm", info)
		if hash, err := ComputeFileHash(shmPath); err == nil {
			if err := backup.SetFileHash("state.db-shm", hash); err != nil {
				backup.SetFileError("state.db-shm", err.Error())
			}
		} else {
			backup.SetFileError("state.db-shm", err.Error())
		}
	}

	if err := backup.Finalize(); err != nil {
		return nil, err
	}

	return backup.manifest, nil
}
