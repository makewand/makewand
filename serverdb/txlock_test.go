package serverdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSQLiteDSNRequestsImmediateTransactions(t *testing.T) {
	if dsn := sqliteDSN("/tmp/state.db"); !strings.Contains(dsn, "_txlock=immediate") {
		t.Fatalf("DSN %q does not request immediate transactions", dsn)
	}
}

// A read-write BeginTx must take the write lock upfront: a second connection
// cannot start its own write transaction while the first is open, even though
// the first has not written anything yet.
func TestBeginTxTakesWriteLockUpfront(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err == nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		t.Fatal("second writer acquired the write lock while a BeginTx transaction was open")
	}

	// Read-only transactions remain deferred and must not take the write lock.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	ro, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(read-only): %v", err)
	}
	defer func() { _ = ro.Rollback() }()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("read-only tx blocked a writer: %v", err)
	}
	_, _ = conn.ExecContext(ctx, `ROLLBACK`)
}

// Concurrent read-modify-write transactions across pool connections must
// neither fail with SQLITE_BUSY on lock upgrade nor lose increments.
func TestConcurrentReadModifyWriteTransactionsDoNotLoseUpdates(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE counter (id INTEGER PRIMARY KEY, n INTEGER NOT NULL); INSERT INTO counter VALUES (1, 0);`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tx, err := db.BeginTx(context.Background(), nil)
				if err != nil {
					errs <- err
					continue
				}
				var n int
				if err := tx.QueryRow(`SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
					_ = tx.Rollback()
					errs <- err
					continue
				}
				if _, err := tx.Exec(`UPDATE counter SET n = ? WHERE id = 1`, n+1); err != nil {
					_ = tx.Rollback()
					errs <- err
					continue
				}
				if err := tx.Commit(); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("transaction error: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if n != workers*perWorker {
		t.Fatalf("counter = %d, want %d (lost updates)", n, workers*perWorker)
	}
}
