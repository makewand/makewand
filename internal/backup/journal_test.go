package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/serverdb"
)

func TestRestoreMultiComponentFailureRollsBackEverything(t *testing.T) {
	src := t.TempDir()
	dbPath := filepath.Join(src, "state.db")
	db, err := serverdb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t(v); INSERT INTO t VALUES('new')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	auth := filepath.Join(src, "auth.json")
	if err := os.WriteFile(auth, []byte("new-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err = Create(archive, Options{StateDBPath: dbPath, AuthConfigPath: auth}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	targetDB := writeLive(t, dst)
	targetAuth := filepath.Join(dst, "auth.json")
	if err := os.WriteFile(targetAuth, []byte("old-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := renameFile
	defer func() { renameFile = old }()
	calls := 0
	renameFile = func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("injected component failure")
		}
		return old(from, to)
	}
	if _, err = Restore(archive, Options{StateDBPath: targetDB, AuthConfigPath: targetAuth}); err == nil {
		t.Fatal("expected failure")
	}
	if readOrEmpty(targetDB) != "LIVE state.db" || readOrEmpty(targetAuth) != "old-auth" {
		t.Fatal("restore left mixed components")
	}
	if readOrEmpty(targetDB+"-wal") != "LIVE state.db-wal" {
		t.Fatal("rollback lost WAL")
	}
}

func TestRestoreJournalPublicationFailureRetainsRecoverableState(t *testing.T) {
	for _, phase := range []string{"prepared", "committed_before_publish", "committed_after_publish"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			auth := filepath.Join(dir, "auth.json")
			secret := filepath.Join(dir, "admin_session_secret")
			for _, path := range []string{auth, secret} {
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			source := filepath.Join(t.TempDir(), "new")
			if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			var installs []restoreInstall
			for _, destination := range []string{auth, secret} {
				tmp, err := stageBeside(source, destination)
				if err != nil {
					t.Fatal(err)
				}
				installs = append(installs, restoreInstall{Name: filepath.Base(destination), Tmp: tmp, Dst: destination})
			}
			originalWriter := persistRestoreJournal
			defer func() { persistRestoreJournal = originalWriter }()
			injected := errors.New("injected journal durability failure")
			persistRestoreJournal = func(path string, journal *restoreJournal) (bool, error) {
				if phase == "committed_before_publish" && journal.State == "committed" {
					return false, injected
				}
				published, err := originalWriter(path, journal)
				if err == nil && ((phase == "prepared" && journal.State == "prepared") || (phase == "committed_after_publish" && journal.State == "committed")) {
					return published, injected
				}
				return published, err
			}
			opts := Options{AuthConfigPath: auth}
			if err := journaledInstall(opts, installs); !errors.Is(err, injected) {
				t.Fatalf("expected injected publication failure: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, restoreJournalName)); err != nil {
				t.Fatalf("recovery journal lost: %v", err)
			}
			persistRestoreJournal = originalWriter
			if err := RecoverRestore(opts); err != nil {
				t.Fatal(err)
			}
			want := "old"
			if phase == "committed_after_publish" {
				want = "new"
			}
			if readOrEmpty(auth) != want || readOrEmpty(secret) != want {
				t.Fatalf("publication recovery mixed states: auth=%q secret=%q want=%q", readOrEmpty(auth), readOrEmpty(secret), want)
			}
			if err := RecoverRestore(opts); err != nil {
				t.Fatalf("recovery is not idempotent: %v", err)
			}
		})
	}
}

func TestRestoreArchiveCannotReplaceRecoveryLock(t *testing.T) {
	dir := t.TempDir()
	reserved := filepath.Join(dir, restoreLockName)
	if err := os.WriteFile(reserved, []byte("archive lock replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(reserved)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBackup()
	b.AddFile(restoreLockName, info)
	hash, err := ComputeFileHash(reserved)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SetFileHash(restoreLockName, hash); err != nil {
		t.Fatal(err)
	}
	if err = b.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err = b.WriteManifest(filepath.Join(dir, archiveManifest)); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "reserved.tar.gz")
	if err = writeTarGz(archive, dir, []string{restoreLockName, archiveManifest}); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	liveLock := filepath.Join(targetDir, restoreLockName)
	if err = os.WriteFile(liveLock, []byte("live lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = Restore(archive, Options{StateDBPath: filepath.Join(targetDir, "state.db")}); err == nil {
		t.Fatal("archive replaced reserved recovery namespace")
	}
	if readOrEmpty(liveLock) != "live lock" {
		t.Fatal("rejection changed live lock")
	}
}

func TestRestoreCrashJournalProcessRecovery(t *testing.T) {
	if os.Getenv("MAKEWAND_RESTORE_CRASH_CHILD") == "1" {
		old := renameFile
		renameFile = func(from, to string) error {
			err := old(from, to)
			if err == nil {
				os.Exit(73)
			}
			return err
		}
		_, err := Restore(os.Getenv("MAKEWAND_RESTORE_CRASH_ARCHIVE"), Options{StateDBPath: os.Getenv("MAKEWAND_RESTORE_CRASH_DB"), AuthConfigPath: os.Getenv("MAKEWAND_RESTORE_CRASH_AUTH")})
		t.Fatal(err)
		return
	}
	src := t.TempDir()
	source := filepath.Join(src, "state.db")
	db, _ := serverdb.Open(source)
	if _, err := db.Exec(`CREATE TABLE t(v); INSERT INTO t VALUES('new')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	auth := filepath.Join(src, "auth.json")
	if err := os.WriteFile(auth, []byte("new-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tar.gz")
	if _, err := Create(archive, Options{StateDBPath: source, AuthConfigPath: auth}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	target := writeLive(t, dst)
	targetAuth := filepath.Join(dst, "auth.json")
	if err := os.WriteFile(targetAuth, []byte("old-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // This launches the trusted current test binary with fixed arguments.
	command := exec.Command(os.Args[0], "-test.run=^TestRestoreCrashJournalProcessRecovery$")
	command.Env = append(os.Environ(), "MAKEWAND_RESTORE_CRASH_CHILD=1", "MAKEWAND_RESTORE_CRASH_ARCHIVE="+archive, "MAKEWAND_RESTORE_CRASH_DB="+target, "MAKEWAND_RESTORE_CRASH_AUTH="+targetAuth, "TMPDIR="+t.TempDir())
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("crash injection: %v", err)
	}
	options := Options{StateDBPath: target, AuthConfigPath: targetAuth}
	if err = RecoverRestore(options); err != nil {
		t.Fatal(err)
	}
	if readOrEmpty(target) != "LIVE state.db" || readOrEmpty(targetAuth) != "old-auth" || readOrEmpty(target+"-wal") != "LIVE state.db-wal" {
		t.Fatal("crash recovery did not restore sealed old components")
	}
	if err = RecoverRestore(options); err != nil {
		t.Fatalf("recovery is not idempotent: %v", err)
	}
}

func TestRestoreRejectsValidHashInvalidSQLiteAndForeignKeys(t *testing.T) {
	for _, kind := range []string{"garbage", "foreign_key", "future_schema"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "state.db")
			if kind == "garbage" {
				if err := os.WriteFile(source, []byte("not sqlite"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, _ := serverdb.Open(source)
				if kind == "foreign_key" {
					if _, err := db.Exec(`PRAGMA foreign_keys=OFF; CREATE TABLE parent(id INTEGER PRIMARY KEY); CREATE TABLE child(id REFERENCES parent(id)); INSERT INTO child VALUES(10)`); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := db.Exec(`PRAGMA user_version=999; CREATE TABLE t(v)`); err != nil {
						t.Fatal(err)
					}
				}
				db.Close()
			}
			info, _ := os.Stat(source)
			descriptor := NewBackup()
			descriptor.AddFile("state.db", info)
			hash, _ := ComputeFileHash(source)
			if err := descriptor.SetFileHash("state.db", hash); err != nil {
				t.Fatal(err)
			}
			if err := descriptor.Finalize(); err != nil {
				t.Fatal(err)
			}
			if err := descriptor.WriteManifest(filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "bad.tar.gz")
			if err := writeTarGz(archive, dir, []string{"state.db", "manifest.json"}); err != nil {
				t.Fatal(err)
			}
			target := writeLive(t, t.TempDir())
			before := readOrEmpty(target)
			if _, err := Restore(archive, Options{StateDBPath: target}); err == nil {
				t.Fatal("invalid DB accepted because hash matched")
			}
			if readOrEmpty(target) != before {
				t.Fatal("rejection changed target")
			}
		})
	}
}

func TestBackupSessionTreeAndSecretRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "tenant"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "tenant", "one.json"), []byte(`{"history":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "admin_session_secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	if _, err := Create(archive, Options{ExtraDirectories: []string{sessions}, ExtraFiles: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "state.db")
	if _, err := Restore(archive, Options{StateDBPath: target}); err != nil {
		t.Fatal(err)
	}
	if readOrEmpty(filepath.Join(filepath.Dir(target), "sessions", "tenant", "one.json")) != `{"history":[]}` {
		t.Fatal("session not restored")
	}
	if readOrEmpty(filepath.Join(filepath.Dir(target), "admin_session_secret")) != "secret" {
		t.Fatal("secret not restored")
	}
}

func TestRestoreRejectsDuplicateArchiveEntries(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "duplicate.tar.gz")
	file, _ := os.Create(archive)
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for i := 0; i < 2; i++ {
		payload := []byte(`{"schema_version":"makewand.backup.v1"}`)
		if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	file.Close()
	if _, err := Restore(archive, Options{StateDBPath: filepath.Join(t.TempDir(), "state.db")}); err == nil {
		t.Fatal("duplicate tar member accepted")
	}
}

func TestRestoreJournalRejectsUnboundDestination(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := restoreJournal{Version: 1, State: "prepared", Originals: []restoreOriginal{{Destination: outside, Existed: false}}}
	data, _ := json.Marshal(j)
	if err := os.WriteFile(filepath.Join(dir, restoreJournalName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	err := RecoverRestore(Options{StateDBPath: filepath.Join(dir, "state.db")})
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unbound journal accepted: %v", err)
	}
	if readOrEmpty(outside) != "keep" {
		t.Fatal("journal deleted external file")
	}
}

func TestRestoreSessionTreeRemovesStaleFiles(t *testing.T) {
	source := t.TempDir()
	sessions := filepath.Join(source, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "new.json"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	if _, err := Create(archive, Options{ExtraDirectories: []string{sessions}}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.MkdirAll(filepath.Join(destination, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "sessions", "stale.json"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(archive, Options{StateDBPath: filepath.Join(destination, "state.db")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "sessions", "stale.json")); !os.IsNotExist(err) {
		t.Fatal("stale session survived complete tree restoration")
	}
	if readOrEmpty(filepath.Join(destination, "sessions", "new.json")) != "new" {
		t.Fatal("new session missing")
	}
}

func TestConcurrentRestoreWritersAreRejected(t *testing.T) {
	directory := t.TempDir()
	lock, err := acquireRestoreLock(filepath.Join(directory, restoreLockName))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	archive := makeArchive(t)
	if _, err = Restore(archive, Options{StateDBPath: filepath.Join(directory, "state.db")}); !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("concurrent restore not rejected: %v", err)
	}
}
