package remotesession

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound indicates the requested remote session does not exist.
var ErrNotFound = errors.New("remote session not found")

// Store persists session blobs on disk.
type Store struct {
	dir string
}

// NewStore creates a file-backed session store rooted at dir.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) pathFor(workspaceID string) (string, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return "", fmt.Errorf("workspace id is empty")
	}
	sum := sha256.Sum256([]byte(workspaceID))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json"), nil
}

// Load returns the stored session blob for workspaceID.
func (s *Store) Load(workspaceID string) ([]byte, error) {
	path, err := s.pathFor(workspaceID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// Save writes a session blob for workspaceID.
func (s *Store) Save(workspaceID string, data []byte) error {
	path, err := s.pathFor(workspaceID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "session-*.tmp")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Delete removes the stored session blob for workspaceID.
func (s *Store) Delete(workspaceID string) error {
	path, err := s.pathFor(workspaceID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
