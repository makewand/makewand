package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/serverdb"
)

func TestValidateSQLiteEscapesLocalPathWithoutModifyingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state % # café.db")
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE preserved(v TEXT); INSERT INTO preserved VALUES('original')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateSQLite(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("read-only validation changed the database: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("validation created sidecar %s: %v", suffix, err)
		}
	}
}
