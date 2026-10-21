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
	quota   Quota
	// freeSpace reports the free bytes of dir's filesystem; nil uses the
	// platform's. Tests inject it.
	freeSpace func(dir string) (uint64, error)
}

// NewStore creates a file-backed session store rooted at dir, without limits.
func NewStore(dir string) *Store {
	return NewStoreWithQuota(dir, Quota{})
}

// NewStoreWithQuota creates a file-backed session store rooted at dir that
// refuses saves beyond quota. Every process sharing dir should use the same
// quota: owner accounting is kept only by stores with per-owner limits.
func NewStoreWithQuota(dir string, quota Quota) *Store {
	store := &Store{dir: dir, quota: quota}
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

// sessionPath names owner's session file for workspaceID. owner is the
// caller's tenant namespace (sessionOwnerKey); the empty owner is the unscoped
// namespace of single-user and identity-less tokens, keyed by the bare ID.
func (s *Store) sessionPath(owner, workspaceID string) (string, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return "", fmt.Errorf("workspace id is empty")
	}
	return s.pathFor(scopedWorkspaceKey(owner, workspaceID))
}

// Load returns the stored session blob for workspaceID in the unscoped
// namespace.
func (s *Store) Load(workspaceID string) ([]byte, error) {
	return s.LoadForOwner("", workspaceID)
}

// LoadForOwner returns owner's stored session blob for workspaceID.
func (s *Store) LoadForOwner(owner, workspaceID string) ([]byte, error) {
	path, err := s.sessionPath(owner, workspaceID)
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

// Save writes a session blob for workspaceID in the unscoped namespace.
func (s *Store) Save(workspaceID string, data []byte) error {
	return s.SaveForOwner("", workspaceID, data)
}

// SaveForOwner writes owner's session blob for workspaceID. A save beyond the
// store's Quota returns a *QuotaError and changes nothing.
func (s *Store) SaveForOwner(owner, workspaceID string, data []byte) error {
	path, err := s.sessionPath(owner, workspaceID)
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
	if err := s.checkFreeSpace(len(data)); err != nil {
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
	if s.quota.perOwner() {
		quotaLock, err := s.lockQuota(owner)
		if err != nil {
			return err
		}
		defer quotaLock.Close()
		ledger, err := s.loadLedger(owner)
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		if _, listed := ledger.Sessions[name]; !listed {
			// A session stored before accounting began (or by a store without
			// per-owner limits) counts at its current size from its next save,
			// so updating it never counts as a new session.
			// #nosec G703 -- path comes from sessionPath, which hashes owner and workspace ID into a basename in the operator-configured storage root.
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
				ledger.Sessions[name] = info.Size()
			}
		}
		if err := s.quota.admit(ledger, name, int64(len(data))); err != nil {
			return err
		}
		// Record before publishing: an interrupted save then leaves an entry
		// the next load repairs, never a session missing from the ledger.
		ledger.Sessions[name] = int64(len(data))
		if err := s.writeLedger(owner, ledger); err != nil {
			return err
		}
	}
	return s.publish(data, path)
}

// publish atomically replaces destination, a file in the store root, with
// data. Callers hold the shared store lock, so Cleanup cannot reap the
// temporary file before it is renamed.
func (s *Store) publish(data []byte, destination string) error {
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
	// #nosec G703 -- source is created by CreateTemp in the trusted root; destination is a SHA-256 basename (pathFor, ledgerPath) in that same root.
	if err := replaceSessionFile(s.dir, tmpName, destination); err != nil {
		// #nosec G703 -- tmpName is returned by CreateTemp in the operator-configured storage root, not supplied by a client.
		_ = os.Remove(tmpName)
		return err
	}
	return syncStoreDirectory(s.dir)
}

// Delete removes the stored session blob for workspaceID in the unscoped
// namespace.
func (s *Store) Delete(workspaceID string) error {
	return s.DeleteForOwner("", workspaceID)
}

// DeleteForOwner removes owner's stored session blob for workspaceID. The
// owner's quota ledger drops the session when it is next loaded.
func (s *Store) DeleteForOwner(owner, workspaceID string) error {
	path, err := s.sessionPath(owner, workspaceID)
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
