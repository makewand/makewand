package serverdb

import (
	"context"
	"database/sql"
	"path/filepath"
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

	for i, c := range conns {
		var fk, busy int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("conn %d read foreign_keys: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("conn %d read busy_timeout: %v", i, err)
		}
		if fk != 1 {
			t.Errorf("conn %d: foreign_keys=%d, want 1", i, fk)
		}
		if busy != 5000 {
			t.Errorf("conn %d: busy_timeout=%d, want 5000", i, busy)
		}
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
}
