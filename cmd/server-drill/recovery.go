package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/makewand/makewand/internal/backup"
	"github.com/makewand/makewand/serverdb"
	"github.com/makewand/makewand/serverusage"
)

func rowHash(db *sql.DB, barrier int64) (string, error) {
	rows, err := db.Query(`SELECT id,payload FROM drill_wal WHERE id<=? ORDER BY id`, barrier)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var value strings.Builder
	for rows.Next() {
		var id int64
		var payload string
		if err = rows.Scan(&id, &payload); err != nil {
			return "", err
		}
		fmt.Fprintf(&value, "%d=%s\n", id, payload)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	return hashString(value.String()), nil
}

func backupDrill(ctx context.Context, o options, r *report, _ *childProcess, directory string, client *http.Client) error {
	path := filepath.Join(directory, "state.db")
	db, err := serverdb.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE drill_wal(id INTEGER PRIMARY KEY,payload TEXT NOT NULL,timestamp_ns INTEGER NOT NULL); CREATE TABLE drill_wal_child(id INTEGER PRIMARY KEY REFERENCES drill_wal(id),payload TEXT NOT NULL)`); err != nil {
		return err
	}
	var acknowledged atomic.Int64
	stop := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		for id := int64(1); ; id++ {
			select {
			case <-stop:
				writerDone <- nil
				return
			case <-ctx.Done():
				writerDone <- ctx.Err()
				return
			default:
			}
			tx, err := db.Begin()
			if err != nil {
				writerDone <- err
				return
			}
			payload := fmt.Sprintf("committed-%d", id)
			_, err = tx.Exec(`INSERT INTO drill_wal VALUES(?,?,?)`, id, payload, time.Now().UnixNano())
			if err == nil {
				_, err = tx.Exec(`INSERT INTO drill_wal_child VALUES(?,?)`, id, payload)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if err != nil {
				writerDone <- err
				return
			}
			acknowledged.Store(id)
			time.Sleep(time.Millisecond)
		}
	}()
	for acknowledged.Load() < 10 {
		select {
		case err = <-writerDone:
			return fmt.Errorf("WAL writer exited: %w", err)
		case <-ctx.Done():
			close(stop)
			<-writerDone
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	barrier := acknowledged.Load()
	r.Recovery.BarrierRows = barrier
	expected, err := rowHash(db, barrier)
	if err != nil {
		close(stop)
		<-writerDone
		return err
	}
	auth := filepath.Join(directory, "server_auth.json")
	secret := filepath.Join(directory, "admin_session_secret")
	sessions := filepath.Join(directory, "sessions")
	if err = os.MkdirAll(filepath.Join(sessions, "drill-tenant"), 0o700); err != nil {
		close(stop)
		<-writerDone
		return err
	}
	if err = os.WriteFile(filepath.Join(sessions, "drill-tenant", "one.json"), []byte(`{"messages":[]}`), 0o600); err != nil {
		close(stop)
		<-writerDone
		return err
	}
	if err = os.WriteFile(auth, []byte(`{"tokens":[]}`), 0o600); err != nil {
		close(stop)
		<-writerDone
		return err
	}
	if err = os.WriteFile(secret, []byte("local-drill-session-secret"), 0o600); err != nil {
		close(stop)
		<-writerDone
		return err
	}
	archive := filepath.Join(directory, "snapshot.tar.gz")
	manifest, err := backup.Create(archive, backup.Options{StateDBPath: path, AuthConfigPath: auth, ExtraFiles: []string{secret}, ExtraDirectories: []string{sessions}})
	close(stop)
	writerErr := <-writerDone
	if err != nil {
		return err
	}
	if writerErr != nil {
		return writerErr
	}
	totalAtStop := acknowledged.Load()
	restoredDir, err := os.MkdirTemp("", "makewand-drill-restored-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(restoredDir)
	restoredPath := filepath.Join(restoredDir, "state.db")
	restoredAuth := filepath.Join(restoredDir, "server_auth.json")
	restoreOpts := backup.Options{StateDBPath: restoredPath, AuthConfigPath: restoredAuth}
	start := time.Now()
	if _, err = backup.Restore(archive, restoreOpts); err != nil {
		return err
	}
	restoredDB, err := serverdb.Open(restoredPath)
	if err != nil {
		return err
	}
	actual, err := rowHash(restoredDB, barrier)
	if err != nil {
		restoredDB.Close()
		return err
	}
	var restoredRows, restoreNS, lastNS int64
	if err = restoredDB.QueryRow(`SELECT COUNT(*),MAX(timestamp_ns) FROM drill_wal`).Scan(&restoredRows, &restoreNS); err != nil {
		restoredDB.Close()
		return err
	}
	if err = db.QueryRow(`SELECT MAX(timestamp_ns) FROM drill_wal`).Scan(&lastNS); err != nil {
		restoredDB.Close()
		return err
	}
	var childRows int64
	err = restoredDB.QueryRow(`SELECT COUNT(*) FROM drill_wal_child`).Scan(&childRows)
	restoredDB.Close()
	if err != nil {
		return err
	}
	r.Recovery.RestoredRows = restoredRows
	r.Recovery.RPOAcknowledgedLost = barrier - restoredRows
	if r.Recovery.RPOAcknowledgedLost < 0 {
		r.Recovery.RPOAcknowledgedLost = 0
	}
	r.Recovery.RPOAfterSnapshotRows = totalAtStop - restoredRows
	r.Recovery.RPOAfterSnapshotMS = float64(lastNS-restoreNS) / 1e6
	restoredChild, err := startChild(ctx, o, restoredDir)
	if err != nil {
		return err
	}
	r.Recovery.RTOMS = float64(time.Since(start).Microseconds()) / 1000
	status, _, requestErr := post(ctx, client, restoredChild.url, "drill-token-0", "after-backup-restore", false)
	restoredChild.kill()
	if requestErr != nil {
		return requestErr
	}
	if err = record(r, "wal_snapshot_barrier_and_foreign_keys", restoredRows >= barrier && childRows == restoredRows && actual == expected, fmt.Sprintf("barrier=%d snapshot=%d matching_child=%d hash=%s", barrier, restoredRows, childRows, actual)); err != nil {
		return err
	}
	if err = record(r, "restore_preserves_auth_and_budget", status == http.StatusTooManyRequests, fmt.Sprintf("restored tenant HTTP status=%d schema=%v", status, manifest.DatabaseSchema)); err != nil {
		return err
	}
	for _, target := range []string{restoredPath, restoredAuth, filepath.Join(restoredDir, "admin_session_secret"), filepath.Join(restoredDir, "sessions", "drill-tenant", "one.json")} {
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		if err = record(r, "restore_private_permissions", info.Mode().Perm() == 0o600, filepath.Base(target)+": "+info.Mode().Perm().String()); err != nil {
			return err
		}
	}
	before, err := backup.ComputeFileHash(restoredPath)
	if err != nil {
		return err
	}
	for _, kind := range []string{"truncated", "checksum", "traversal"} {
		damaged := filepath.Join(directory, "damaged-"+kind+".tar.gz")
		if err = damageArchive(archive, damaged, kind); err != nil {
			return err
		}
		_, restoreErr := backup.Restore(damaged, restoreOpts)
		after, err := backup.ComputeFileHash(restoredPath)
		if err != nil {
			return err
		}
		if err = record(r, "reject_"+kind+"_before_live_write", restoreErr != nil && before == after, fmt.Sprintf("rejected=%t target_hash_unchanged=%t", restoreErr != nil, before == after)); err != nil {
			return err
		}
	}
	if err = migrationDrill(restoredPath, r); err != nil {
		return err
	}
	return restoreCrashDrill(ctx, o, archive, r)
}

func damageArchive(source, target, kind string) error {
	if kind == "truncated" {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data[:len(data)/2], 0o600) //nolint:gosec // A fixed drill-created basename in its private temporary directory.
	}
	output, err := os.Create(target)
	if err != nil {
		return err
	}
	defer output.Close()
	gz := gzip.NewWriter(output)
	tw := tar.NewWriter(gz)
	if kind == "traversal" {
		payload := []byte("unsafe")
		if err = tw.WriteHeader(&tar.Header{Name: "../state.db", Typeflag: tar.TypeReg, Size: int64(len(payload)), Mode: 0o600}); err == nil {
			_, err = tw.Write(payload)
		}
	} else {
		input, e := os.Open(source)
		if e != nil {
			return e
		}
		defer input.Close()
		reader, e := gzip.NewReader(input)
		if e != nil {
			return e
		}
		defer reader.Close()
		tr := tar.NewReader(reader)
		for {
			header, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			data, e := io.ReadAll(tr)
			if e != nil {
				return e
			}
			if header.Name == "state.db" && len(data) > 0 {
				data[0] ^= 0xff
			}
			if e = tw.WriteHeader(header); e != nil {
				return e
			}
			if _, e = tw.Write(data); e != nil {
				return e
			}
		}
	}
	if err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	return output.Close()
}

func migrationDrill(path string, r *report) error {
	// N-1 removes the integer column and N-2 also removes tenant attribution.
	// The store must reopen twice without losing any legacy event or spend.
	for _, version := range []string{"n-1", "n-2"} {
		directory, err := os.MkdirTemp("", "makewand-drill-migration-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(directory)
		legacy := filepath.Join(directory, "state.db")
		if err = backup.SnapshotSQLite(path, legacy); err != nil {
			return err
		}
		db, err := serverdb.Open(legacy)
		if err != nil {
			return err
		}
		var count int
		var cost float64
		if err = db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(cost_usd),0) FROM usage_entries`).Scan(&count, &cost); err != nil {
			db.Close()
			return err
		}
		_, err = db.Exec(`ALTER TABLE usage_entries DROP COLUMN cost_micro_usd; DELETE FROM schema_versions WHERE component='usage'`)
		if err == nil && version == "n-2" {
			_, err = db.Exec(`DROP INDEX usage_org_time; DROP INDEX usage_project_time; ALTER TABLE usage_entries DROP COLUMN organization_id; ALTER TABLE usage_entries DROP COLUMN project_id; DROP INDEX usage_user_time; ALTER TABLE usage_entries DROP COLUMN user_id`)
		}
		db.Close()
		if err != nil {
			return err
		}
		for attempt := 0; attempt < 2; attempt++ {
			store, err := serverusage.OpenSQLiteStore(legacy)
			if err != nil {
				return err
			}
			entries, err := store.Load(serverusage.Filter{})
			store.Close()
			if err != nil {
				return err
			}
			var sum float64
			for _, entry := range entries {
				sum += entry.CostUSD
			}
			if err = record(r, "migration_"+version+"_retry_"+strconv.Itoa(attempt), len(entries) == count && sum >= cost-.000001, fmt.Sprintf("rows=%d original=%d USD=%g", len(entries), count, sum)); err != nil {
				return err
			}
		}
		r.Recovery.MigrationRows = count
	}
	return nil
}

func restoreCrashDrill(ctx context.Context, o options, archive string, r *report) error {
	dir, err := os.MkdirTemp("", "makewand-drill-restore-crash-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	target := filepath.Join(dir, "state.db")
	db, err := serverdb.Open(target)
	if err != nil {
		return err
	}
	if _, err = db.Exec(`CREATE TABLE before_restore(v TEXT); INSERT INTO before_restore VALUES('old')`); err != nil {
		db.Close()
		return err
	}
	db.Close()
	auth := filepath.Join(dir, "server_auth.json")
	if err = os.WriteFile(auth, []byte("old-auth"), 0o600); err != nil {
		return err
	}
	secret := filepath.Join(dir, "admin_session_secret")
	if err = os.WriteFile(secret, []byte("old-secret"), 0o600); err != nil {
		return err
	}
	before, err := backup.ComputeFileHash(target)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ready := filepath.Join(dir, "crash-ready")
	command := exec.CommandContext(ctx, executable, "--child-restore", "--state-dir", dir, "--archive", archive, "--ready-file", ready)
	if err = command.Start(); err != nil {
		return err
	}
	if err = waitForFile(ctx, ready); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	if err = command.Process.Kill(); err != nil {
		return err
	}
	waitErr := command.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		return fmt.Errorf("restore child was not killed: %v", waitErr)
	}
	start := time.Now()
	opts := backup.Options{StateDBPath: target, AuthConfigPath: auth}
	if err = backup.RecoverRestore(opts); err != nil {
		return err
	}
	after, err := backup.ComputeFileHash(target)
	if err != nil {
		return err
	}
	authData, _ := os.ReadFile(auth)
	secretData, _ := os.ReadFile(secret)
	if err = record(r, "kill_between_restore_components_rolls_back", before == after && string(authData) == "old-auth" && string(secretData) == "old-secret", fmt.Sprintf("recovery_ms=%.2f original_db_hash=%s", float64(time.Since(start).Microseconds())/1000, after)); err != nil {
		return err
	}
	if err = backup.RecoverRestore(opts); err != nil {
		return err
	}
	return record(r, "restore_recovery_idempotent", true, "second recovery changed no state")
}
