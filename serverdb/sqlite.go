package serverdb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// busy_timeout and foreign_keys are per-CONNECTION settings. database/sql
	// pools connections and opens new ones on demand, so setting them with a
	// single db.Exec would configure only whichever connection happened to run
	// it — every other pooled connection would silently get busy_timeout=0 and
	// foreign_keys=OFF. modernc.org/sqlite applies `_pragma` DSN options to every
	// connection it opens, so encode them there instead. journal_mode=WAL is a
	// persistent database-level setting, so a one-time Exec below is sufficient.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
