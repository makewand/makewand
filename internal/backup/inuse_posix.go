//go:build unix

package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// sqliteShmDMSOffset is the "dead man switch" byte in a WAL-mode database's
// -shm file (UNIX_SHM_BASE + SQLITE_SHM_NLOCK in SQLite's unix VFS). Every open
// connection holds a shared lock on it for as long as it has the database open.
const sqliteShmDMSOffset = 128

// stateDBInUse reports whether another connection currently has the WAL-mode
// SQLite database at dbPath open (for example a running makewand server).
func stateDBInUse(dbPath string) (bool, error) {
	shm, err := os.Open(dbPath + "-shm")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil // no WAL index: no connection has it open
		}
		return false, fmt.Errorf("inspect %s-shm: %w", dbPath, err)
	}
	defer shm.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: sqliteShmDMSOffset, Len: 1}
	if err := queryWriteLock(shm.Fd(), &lk); err != nil {
		return false, fmt.Errorf("probe %s-shm lock: %w", dbPath, err)
	}
	return lk.Type != syscall.F_UNLCK, nil
}
