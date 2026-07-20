package backup_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/makewand/makewand/internal/backup"
	"github.com/makewand/makewand/serverdb"
)

func seedDB(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec("INSERT INTO t(v) VALUES(?)", fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

// TestBackupRestoreRoundTrip backs up a live database (with a concurrent writer)
// and confirms the restored copy is internally consistent and complete.
func TestBackupRestoreRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	stateDB := filepath.Join(srcDir, "state.db")
	authCfg := filepath.Join(srcDir, "server_auth.json")
	seedDB(t, stateDB, 50)
	if err := os.WriteFile(authCfg, []byte(`{"tokens":[]}`), 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	// Keep a live connection writing during the backup to exercise VACUUM INTO's
	// consistency against a concurrently-modified database.
	live, err := serverdb.Open(stateDB)
	if err != nil {
		t.Fatalf("open live db: %v", err)
	}
	defer live.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				_, _ = live.Exec("INSERT INTO t(v) VALUES(?)", fmt.Sprintf("live-%d", i))
			}
		}
	}()

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	manifest, err := backup.Create(archive, backup.Options{StateDBPath: stateDB, AuthConfigPath: authCfg})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(manifest.Files) != 2 {
		t.Fatalf("manifest files = %d, want 2 (state.db + server_auth.json)", len(manifest.Files))
	}

	dstDir := t.TempDir()
	dstDB := filepath.Join(dstDir, "state.db")
	dstAuth := filepath.Join(dstDir, "server_auth.json")
	if _, err := backup.Restore(archive, backup.Options{StateDBPath: dstDB, AuthConfigPath: dstAuth}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rdb, err := serverdb.Open(dstDB)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer rdb.Close()
	var integrity string
	if err := rdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
	var count int
	if err := rdb.QueryRow("SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count < 50 {
		t.Fatalf("restored row count = %d, want >= 50 (the seeded rows)", count)
	}

	data, err := os.ReadFile(dstAuth)
	if err != nil {
		t.Fatalf("read restored auth: %v", err)
	}
	if string(data) != `{"tokens":[]}` {
		t.Fatalf("restored auth = %q, want the original auth config", string(data))
	}
}

// TestRestoreRejectsCorruptArchive confirms a damaged archive fails rather than
// installing garbage over live state.
func TestRestoreRejectsCorruptArchive(t *testing.T) {
	srcDir := t.TempDir()
	stateDB := filepath.Join(srcDir, "state.db")
	seedDB(t, stateDB, 5)

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := backup.Create(archive, backup.Options{StateDBPath: stateDB}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Truncate the archive to corrupt it.
	if err := os.Truncate(archive, 32); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	dstDB := filepath.Join(t.TempDir(), "state.db")
	if _, err := backup.Restore(archive, backup.Options{StateDBPath: dstDB}); err == nil {
		t.Fatal("Restore should fail on a corrupt archive, got nil")
	}
	if _, err := os.Stat(dstDB); err == nil {
		t.Fatal("Restore must not install any file from a corrupt archive")
	}
}
