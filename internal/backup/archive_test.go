package backup_test

import (
	"archive/tar"
	"compress/gzip"
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

// TestRestoreRejectsOversizedEntry confirms decompression bomb defenses reject
// files that expand beyond the configured safe entry limit.
func TestRestoreRejectsOversizedEntry(t *testing.T) {
	srcDir := t.TempDir()
	stateDB := filepath.Join(srcDir, "state.db")
	seedDB(t, stateDB, 5)

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := backup.Create(archive, backup.Options{StateDBPath: stateDB}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set limit to 10 bytes to trigger decompression bomb protection.
	cleanup := backup.SetMaxArchiveLimitsForTest(10, 1000)
	defer cleanup()

	dstDB := filepath.Join(t.TempDir(), "state.db")
	_, err := backup.Restore(archive, backup.Options{StateDBPath: dstDB})
	if err == nil {
		t.Fatal("Restore should fail when entry exceeds max allowed size, got nil")
	}
	if _, err := os.Stat(dstDB); err == nil {
		t.Fatal("Restore must not leave oversized files on disk")
	}
}

// TestRestoreRejectsExcessiveEntryCount confirms archives with more entries than
// maxArchiveEntryCount are rejected to prevent inode exhaustion.
func TestRestoreRejectsExcessiveEntryCount(t *testing.T) {
	srcDir := t.TempDir()
	stateDB := filepath.Join(srcDir, "state.db")
	seedDB(t, stateDB, 5)

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := backup.Create(archive, backup.Options{StateDBPath: stateDB}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set limit to 1 entry (a valid backup has at least manifest.json + state.db = 2 entries).
	cleanup := backup.SetMaxArchiveLimitsForTest(1024*1024, 10*1024*1024, 1)
	defer cleanup()

	dstDB := filepath.Join(t.TempDir(), "state.db")
	_, err := backup.Restore(archive, backup.Options{StateDBPath: dstDB})
	if err == nil {
		t.Fatal("Restore should fail when entry count exceeds limit, got nil")
	}
	if _, err := os.Stat(dstDB); err == nil {
		t.Fatal("Restore must not install files from archive exceeding entry limit")
	}
}

// TestRestoreRejectsManifestTraversal verifies that a malicious manifest.json with
// directory traversal paths cannot write files outside the destination directory.
func TestRestoreRejectsManifestTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "malicious.tar.gz")

	// Create an archive containing a crafted manifest.json with traversal filename.
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	badManifest := []byte(`{
		"version": 1,
		"created_at": "2026-09-28T00:00:00Z",
		"files": [
			{"name": "../../evil.txt", "size": 4, "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
		]
	}`)

	hdr := &tar.Header{
		Name:     "manifest.json",
		Mode:     0o600,
		Size:     int64(len(badManifest)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(badManifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	stateDir := filepath.Join(tmpDir, "target_state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	escapedFile := filepath.Join(tmpDir, "evil.txt")
	_, err = backup.Restore(archivePath, backup.Options{
		StateDBPath: filepath.Join(stateDir, "state.db"),
	})
	if err == nil {
		t.Fatal("Restore must fail on malicious manifest containing path traversal, got nil")
	}
	if _, err := os.Stat(escapedFile); err == nil {
		t.Fatalf("Path traversal succeeded: %s was created!", escapedFile)
	}
}
