package remotesession

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ErrNotFound indicates the requested remote session does not exist.
var ErrNotFound = errors.New("remote session not found")

// Store persists session blobs on disk.
type Store struct {
	dir     string
	cleaned atomic.Bool
}

// NewStore creates a file-backed session store rooted at dir.
func NewStore(dir string) *Store {
	store := &Store{dir: dir}
	// Construction cannot report storage errors. Try housekeeping without
	// waiting for another process's active writer; Save retries and reports
	// errors if this first attempt was unavailable.
	_ = store.cleanupIfIdle()
	return store
}

// Cleanup removes temporary files left by interrupted saves. It waits for all
// cooperating writers in this directory, including writers in other processes.
// The lock file is permanent: replacing it would split the lock namespace.
func (s *Store) Cleanup() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	lock, err := openStoreLock(filepath.Join(s.dir, ".session.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockStoreHandle(lock, true); err != nil {
		return err
	}
	if err := s.removeSessionTemps(); err != nil {
		return err
	}
	s.cleaned.Store(true)
	return nil
}

func (s *Store) cleanupIfIdle() error {
	if s.dir == "" || s.cleaned.Load() {
		return nil
	}
	lock, err := openStoreLock(filepath.Join(s.dir, ".session.lock"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	acquired, err := tryExclusiveStoreLock(lock)
	if err != nil || !acquired {
		return err
	}
	if !s.cleaned.Load() {
		if err := s.removeSessionTemps(); err != nil {
			return err
		}
		s.cleaned.Store(true)
	}
	return nil
}

// removeSessionTemps requires an exclusive store lock. Every Save holds a
// shared lock before creating its temporary file until after the final rename.
func (s *Store) removeSessionTemps() error {
	// #nosec G703 -- dir is the operator-configured session storage root; clients only supply workspace IDs, which pathFor hashes into basenames.
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	removed := false
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			// Never follow links or remove directories with a matching name.
			if !info.Mode().IsRegular() {
				continue
			}
			// #nosec G703 -- ReadDir supplies a basename in the service-owned root; an exclusive store lock protects removal of regular session-*.tmp files.
			if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
				return err
			}
			removed = true
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			break
		}
	}
	if removed {
		return syncStoreDirectory(s.dir)
	}
	return nil
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
	data, err := readSessionFile(path)
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
	// #nosec G703 -- dir is the operator-configured session storage root, independent of client workspace IDs.
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := s.cleanupIfIdle(); err != nil {
		return err
	}
	lock, err := openStoreLock(filepath.Join(s.dir, ".session.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockStoreHandle(lock, false); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "session-*.tmp")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		// #nosec G703 -- tmpName is returned by CreateTemp in the operator-configured storage root, not supplied by a client.
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		// #nosec G703 -- tmpName is returned by CreateTemp in the operator-configured storage root, not supplied by a client.
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		// #nosec G703 -- tmpName is returned by CreateTemp in the operator-configured storage root, not supplied by a client.
		_ = os.Remove(tmpName)
		return err
	}
	// #nosec G703 -- source is created by CreateTemp in the trusted root; destination is the SHA-256 basename returned by pathFor in that same root.
	if err := replaceSessionFile(s.dir, tmpName, path); err != nil {
		// #nosec G703 -- tmpName is returned by CreateTemp in the operator-configured storage root, not supplied by a client.
		_ = os.Remove(tmpName)
		return err
	}
	return syncStoreDirectory(s.dir)
}

// Delete removes the stored session blob for workspaceID.
func (s *Store) Delete(workspaceID string) error {
	path, err := s.pathFor(workspaceID)
	if err != nil {
		return err
	}
	if err := removeSessionFile(s.dir, path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncStoreDirectory(s.dir)
}
