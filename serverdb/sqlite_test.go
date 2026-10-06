package serverdb

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestOpenPragmasApplyToEveryConnection guards the per-connection PRAGMA fix:
// busy_timeout and foreign_keys must be set on every pooled connection, not just
// the one that happened to run a startup Exec. Holding several connections at
// once forces database/sql to open distinct connections; each must report the
// configured values.
func TestOpenPragmasApplyToEveryConnection(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(4)
	ctx := context.Background()

	// Grab and hold 4 connections simultaneously so the pool must open 4 distinct
	// underlying connections rather than reusing one.
	conns := make([]*sql.Conn, 0, 4)
	for i := 0; i < 4; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	for _, c := range conns {
		assertConnectionPragmas(t, c)
	}
}

func assertConnectionPragmas(t *testing.T, conn *sql.Conn) {
	t.Helper()
	for pragma, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 30000, "synchronous": 1} {
		var got int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("%s=%d, want %d", pragma, got, want)
		}
	}
}

func TestOpenPreservesFilenameAndPrivateFiles(t *testing.T) {
	filenames := []string{"state#review.db", "state%review.db", "state%2Freview.db", "state file.db", "数据库.db"}
	if runtime.GOOS != "windows" {
		// A question mark is a legal filename character on Unix, but not Windows.
		filenames = append(filenames, "state?review.db", "state?_pragma=foreign_keys(0)#% review.db")
	}
	for _, filename := range filenames {
		for _, relative := range []bool{false, true} {
			name := filename + "/absolute"
			if relative {
				name = filename + "/relative"
			}
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), filename)
				openPath := path
				if relative {
					// The checkout and temp directory can be on different Windows
					// drives, so create the relative path from its own directory.
					t.Chdir(filepath.Dir(path))
					openPath = filename
				}
				db, err := Open(openPath)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer db.Close()
				var actualPath string
				if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&actualPath); err != nil {
					t.Fatalf("database_list: %v", err)
				}
				requested, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat requested file: %v", err)
				}
				actual, err := os.Stat(actualPath)
				if err != nil || !os.SameFile(requested, actual) {
					t.Fatalf("SQLite opened %q instead of %q: %v", actualPath, path, err)
				}
				if _, err := db.Exec(`CREATE TABLE filename_probe(value TEXT); INSERT INTO filename_probe VALUES ('saved')`); err != nil {
					t.Fatalf("write: %v", err)
				}
				for _, suffix := range []string{"", "-wal", "-shm"} {
					info, err := os.Stat(path + suffix)
					if err != nil {
						t.Fatalf("stat database%s: %v", suffix, err)
					}
					if info.Size() == 0 {
						t.Errorf("database%s is empty", suffix)
					}
					if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
						t.Errorf("database%s permissions=%04o, want 0600", suffix, info.Mode().Perm())
					}
				}
				// Force another physical connection so URI encoding cannot accidentally
				// drop any per-connection PRAGMA while fixing the file path.
				db.SetMaxOpenConns(2)
				var conns []*sql.Conn
				for range 2 {
					conn, err := db.Conn(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					conns = append(conns, conn)
					defer conn.Close()
					assertConnectionPragmas(t, conn)
				}
				for _, conn := range conns {
					_ = conn.Close()
				}
				backupPath := filepath.Join(filepath.Dir(path), "backup #%.db")
				if _, err := db.Exec(`VACUUM INTO ?`, backupPath); err != nil {
					t.Fatalf("snapshot: %v", err)
				}
				if err := db.Close(); err != nil {
					t.Fatalf("close before reopen: %v", err)
				}
				for _, reopenPath := range []string{openPath, backupPath} {
					reopened, err := Open(reopenPath)
					if err != nil {
						t.Fatalf("reopen %q: %v", reopenPath, err)
					}
					var value string
					err = reopened.QueryRow(`SELECT value FROM filename_probe`).Scan(&value)
					_ = reopened.Close()
					if err != nil || value != "saved" {
						t.Fatalf("reopen %q: value=%q, error=%v", reopenPath, value, err)
					}
				}
			})
		}
	}
}

func TestSQLiteDSNWindowsPaths(t *testing.T) {
	// Forward slashes allow drive and UNC URI structure to be checked on every
	// host. Native Windows Open tests above also exercise backslash conversion.
	for _, path := range []string{"C:/makewand-testdata/state #%.db", "//server/share/state #%.db"} {
		t.Run(path, func(t *testing.T) {
			uri, err := url.Parse(sqliteDSN(path))
			if err != nil {
				t.Fatal(err)
			}
			wantPath := path
			if path[0] != '/' {
				wantPath = "/" + path
			}
			if uri.Scheme != "file" || uri.Host != "" || uri.Path != wantPath || uri.Fragment != "" {
				t.Fatalf("incorrect file URI: scheme=%q host=%q path=%q fragment=%q", uri.Scheme, uri.Host, uri.Path, uri.Fragment)
			}
		})
	}
}

// TestOpenEnablesWAL confirms the database-level journal mode is WAL.
func TestOpenEnablesWAL(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode=%q, want wal", mode)
	}

	// Test checkpoint execution
	if err := Checkpoint(db); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Test checkpoint loop start and stop
	stop := StartWALCheckpointLoop(context.Background(), db, 50)
	stop()
}
