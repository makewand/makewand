package serverdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Open opens a SQLite database path with strict parent directory permissions.
func Open(path string) (*sql.DB, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("sqlite path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("sqlite path is not a regular file: %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// SQLite's default mode depends on umask; create the credential/usage DB
	// privately before the driver can create a world-readable state file.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	// busy_timeout, foreign_keys and synchronous are per-CONNECTION settings.
	// modernc.org/sqlite applies `_pragma` DSN options to every connection it opens,
	// so encode them there instead of single db.Exec. journal_mode=WAL is a
	// persistent database-level setting, configured via Exec below.
	dsn := path + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Serialize connections to 1 to eliminate write-lock contention under concurrent goroutines.
	db.SetMaxOpenConns(1)
	return db, nil
}

// Checkpoint performs a passive WAL checkpoint (PRAGMA wal_checkpoint(PASSIVE)).
func Checkpoint(db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`PRAGMA wal_checkpoint(PASSIVE);`)
	return err
}

// StartWALCheckpointLoop starts a periodic goroutine that performs passive WAL checkpoints.
// Returns a stop function that waits for the loop to terminate and runs a final checkpoint.
func StartWALCheckpointLoop(ctx context.Context, db *sql.DB, interval time.Duration) func() {
	if db == nil || interval <= 0 {
		return func() {}
	}
	checkpointCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-checkpointCtx.Done():
				return
			case <-ticker.C:
				_ = Checkpoint(db)
			}
		}
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			wg.Wait()
			_ = Checkpoint(db)
		})
	}
}
