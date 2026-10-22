package serverusage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/serverdb"
)

// go-server-1021-5: settled reservations no longer accumulate one row per
// request forever.

func openBudgetTestStore(t *testing.T) (*SQLiteStore, *sql.DB) {
	t.Helper()
	// Hundreds of transactions: skip the per-commit fsync of the default FULL.
	t.Setenv("MAKEWAND_SQLITE_SYNCHRONOUS", "NORMAL")
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLiteStoreWithDB(db, path)
	if err != nil {
		t.Fatal(err)
	}
	return store, db
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const (
	countReservations = `SELECT COUNT(*) FROM budget_reservations`
	countScopes       = `SELECT COUNT(*) FROM budget_reservation_scopes`
	countUsage        = `SELECT COUNT(*) FROM usage_entries`
)

func TestSettledReservationsArePrunedSoTheTableStaysBounded(t *testing.T) {
	store, db := openBudgetTestStore(t)
	now := time.Now().UTC()
	month := now.Format("2006-01")
	scopes := []BudgetScope{{Kind: "token_month", ID: "t", Period: month, LimitMicroUSD: 1_000_000_000}, {Kind: "org_month", ID: "o", Period: month, LimitMicroUSD: 1_000_000_000}}
	settle := func(id string, expires time.Time) {
		t.Helper()
		if err := store.ReserveBudget(context.Background(), BudgetReservation{ID: id, EstimateMicroUSD: 10_000, ExpiresAt: expires, Scopes: scopes}); err != nil {
			t.Fatal(err)
		}
		if err := store.Log(Entry{Timestamp: now, ReservationID: id, TokenID: "t", OrganizationID: "o", CostUSD: .001}); err != nil {
			t.Fatal(err)
		}
	}
	// Requests whose reservations expired over a day ago: each settlement
	// deletes the ones settled before it.
	for i := range 300 {
		settle(fmt.Sprintf("old-%03d", i), now.Add(-settledReservationRetention-time.Hour))
	}
	if got := countRows(t, db, countReservations); got != 1 {
		t.Fatalf("budget_reservations rows after 300 settled requests = %d, want 1", got)
	}
	if got := countRows(t, db, countScopes); got != len(scopes) {
		t.Fatalf("budget_reservation_scopes rows = %d, want %d", got, len(scopes))
	}
	if got := countRows(t, db, countUsage); got != 300 {
		t.Fatalf("usage entries = %d, want all 300 kept", got)
	}

	// A recent settled reservation stays: settling it again is a no-op and
	// its ID cannot be reserved again.
	settle("recent", now.Add(15*time.Minute))
	if err := store.Log(Entry{Timestamp: now, ReservationID: "recent", TokenID: "t", OrganizationID: "o", CostUSD: .001}); err != nil {
		t.Fatalf("repeated settlement: %v", err)
	}
	if got := countRows(t, db, countUsage); got != 301 {
		t.Fatalf("usage entries = %d, want 301: the repeated settlement was recorded twice", got)
	}
	if err := store.ReserveBudget(context.Background(), BudgetReservation{ID: "recent", EstimateMicroUSD: 1, ExpiresAt: now.Add(time.Minute), Scopes: scopes}); err != ErrReservationExists {
		t.Fatalf("reserving a recently settled ID again = %v, want ErrReservationExists", err)
	}

	// Budget arithmetic is unchanged: only settled spend remains charged.
	var settled, reserved int64
	if err := db.QueryRow(`SELECT settled_micro_usd,reserved_micro_usd FROM budget_accounts WHERE kind='org_month' AND scope_id='o' AND period=?`, month).Scan(&settled, &reserved); err != nil {
		t.Fatal(err)
	}
	if settled != 301*1000 || reserved != 0 {
		t.Fatalf("org account settled=%d reserved=%d, want %d and 0", settled, reserved, 301*1000)
	}
}

// A backlog of settled reservations from before pruning drains in bounded
// batches; pending and expired reservations, which hold budget, stay.
func TestSettledReservationBacklogDrainsInBatches(t *testing.T) {
	store, db := openBudgetTestStore(t)
	old := time.Now().UTC().Add(-settledReservationRetention - time.Hour).Format(sinceBoundaryLayout)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id, state string) {
		t.Helper()
		if _, err := tx.Exec(`INSERT INTO budget_reservations(request_id,estimate_micro_usd,expires_at,state) VALUES(?,?,?,?)`, id, 1000, old, state); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO budget_reservation_scopes(request_id,kind,scope_id,period) VALUES(?,?,?,?)`, id, "token_month", "t", "2026-09"); err != nil {
			t.Fatal(err)
		}
	}
	backlog := 2*pruneSettledBatch + 10
	for i := range backlog {
		insert(fmt.Sprintf("legacy-%04d", i), "settled")
	}
	insert("pending", "pending")
	insert("crashed", "expired")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for logged, want := range []int{backlog - pruneSettledBatch, backlog - 2*pruneSettledBatch, 0} {
		if err := store.Log(Entry{Timestamp: time.Now().UTC(), RequestID: fmt.Sprint(logged)}); err != nil {
			t.Fatal(err)
		}
		var settled int
		if err := db.QueryRow(`SELECT COUNT(*) FROM budget_reservations WHERE state='settled'`).Scan(&settled); err != nil {
			t.Fatal(err)
		}
		if settled != want {
			t.Fatalf("after %d usage records: %d settled reservations, want %d", logged+1, settled, want)
		}
	}
	if got := countRows(t, db, countReservations); got != 2 {
		t.Fatalf("reservations left = %d, want the pending and the expired one", got)
	}
	if got := countRows(t, db, countScopes); got != 2 {
		t.Fatalf("scopes left = %d, want those of the pending and the expired reservation", got)
	}
}

// A settled reservation is pruned a day after it expires. A settlement repeated
// after that finds no reservation: it is refused, and its transaction rolls
// back, so it records no second usage row and charges nothing again.
func TestRepeatedSettlementAfterPruneIsRefusedAndChargesNothing(t *testing.T) {
	store, db := openBudgetTestStore(t)
	now := time.Now().UTC()
	month := now.Format("2006-01")
	scopes := []BudgetScope{{Kind: "token_month", ID: "t", Period: month, LimitMicroUSD: 1_000_000_000}}
	if err := store.ReserveBudget(context.Background(), BudgetReservation{ID: "late", EstimateMicroUSD: 10_000, ExpiresAt: now.Add(-settledReservationRetention - time.Hour), Scopes: scopes}); err != nil {
		t.Fatal(err)
	}
	entry := Entry{Timestamp: now, ReservationID: "late", TokenID: "t", CostUSD: .001}
	if err := store.Log(entry); err != nil {
		t.Fatal(err)
	}
	charged := func() (settled, reserved int64) {
		t.Helper()
		if err := db.QueryRow(`SELECT settled_micro_usd,reserved_micro_usd FROM budget_accounts WHERE kind='token_month' AND scope_id='t' AND period=?`, month).Scan(&settled, &reserved); err != nil {
			t.Fatal(err)
		}
		return settled, reserved
	}
	refused := func(usageRows int) {
		t.Helper()
		if err := store.Log(entry); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("a settlement repeated after pruning = %v, want it refused", err)
		}
		if got := countRows(t, db, countUsage); got != usageRows {
			t.Fatalf("usage entries = %d, want %d", got, usageRows)
		}
		if settled, reserved := charged(); settled != 1000 || reserved != 0 {
			t.Fatalf("token account settled=%d reserved=%d, want 1000 and 0", settled, reserved)
		}
	}
	// The repeat prunes the settled reservation in its own transaction, finds
	// none, and rolls back, prune included.
	refused(1)
	if got := countRows(t, db, countReservations); got != 1 {
		t.Fatalf("reservations = %d, want the rolled-back prune undone", got)
	}
	// Another usage write prunes it for good; a repeat is still refused.
	if err := store.Log(Entry{Timestamp: now, RequestID: "next"}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, countReservations); got != 0 {
		t.Fatalf("reservations = %d, want the settled one pruned", got)
	}
	refused(2)
}
