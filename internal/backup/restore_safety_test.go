package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/serverdb"
)

func makeArchive(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "state.db")
	db, err := serverdb.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('backup')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(archive, Options{StateDBPath: src}); err != nil {
		t.Fatal(err)
	}
	return archive
}

func writeLive(t *testing.T, dir string) string {
	t.Helper()
	dst := filepath.Join(dir, "state.db")
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("LIVE "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func readOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// go-engine-tui-cmd#8: the -wal/-shm sidecars were deleted BEFORE the new
// database was installed, so a failed install lost the WAL's committed
// transactions. A failing swap must leave the live database and its sidecars
// exactly as they were.
func TestRestore_FailedInstallKeepsLiveDatabaseAndWAL(t *testing.T) {
	archive := makeArchive(t)
	dir := t.TempDir()
	dst := writeLive(t, dir)

	oldRename := renameFile
	t.Cleanup(func() { renameFile = oldRename })
	renameFile = func(from, to string) error {
		if to == dst {
			return errors.New("injected: disk full")
		}
		return oldRename(from, to)
	}

	if _, err := Restore(archive, Options{StateDBPath: dst}); err == nil {
		t.Fatal("Restore succeeded despite the injected install failure")
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
		if got := readOrEmpty(filepath.Join(dir, name)); got != "LIVE "+name {
			t.Fatalf("%s = %q after a failed restore, want the untouched live file", name, got)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 || readOrEmpty(filepath.Join(dir, restoreLockName)) != "" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed restore left staging/parked files behind: %v", names)
	}
}

// The original repro: a directory squatting on the old fixed staging name.
func TestRestore_StagingNameCollisionDoesNotTouchLiveFiles(t *testing.T) {
	archive := makeArchive(t)
	dir := t.TempDir()
	dst := writeLive(t, dir)
	if err := os.Mkdir(dst+".restore.tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(archive, Options{StateDBPath: dst}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Success: the restored snapshot is in place and the stale sidecars of the
	// replaced database are gone (they must never be replayed onto it).
	db, err := serverdb.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow("SELECT v FROM t").Scan(&v); err != nil || v != "backup" {
		t.Fatalf("restored database content = %q, %v", v, err)
	}
}

func TestRestore_RefusesWhileDatabaseIsOpen(t *testing.T) {
	archive := makeArchive(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "state.db")
	live, err := serverdb.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Exec("CREATE TABLE live (v TEXT); INSERT INTO live VALUES ('keep')"); err != nil {
		t.Fatal(err)
	}
	_, err = Restore(archive, Options{StateDBPath: dst})
	if !errors.Is(err, ErrStateDBInUse) || !strings.Contains(err.Error(), "server") {
		t.Fatalf("Restore on an open database: err = %v, want ErrStateDBInUse", err)
	}
	var v string
	if err := live.QueryRow("SELECT v FROM live").Scan(&v); err != nil || v != "keep" {
		t.Fatalf("live database disturbed by the refused restore: %q %v", v, err)
	}
	live.Close()

	// Once the server is stopped the restore proceeds.
	if _, err := Restore(archive, Options{StateDBPath: dst}); err != nil {
		t.Fatalf("Restore after closing the database: %v", err)
	}
}
