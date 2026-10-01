package serverusage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/serverdb"
)

func TestDurableBudgetConcurrentIndependentStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	stores := make([]*SQLiteStore, 4)
	for i := range stores {
		var err error
		stores[i], err = OpenSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
	}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	now := time.Now().UTC()
	scope := BudgetScope{Kind: "org_month", ID: "tenant", Period: now.Format("2006-01"), LimitMicroUSD: 1_000_000}
	errs := make(chan error, 1000)
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := stores[i%len(stores)].ReserveBudget(context.Background(), BudgetReservation{ID: fmt.Sprint(i), EstimateMicroUSD: 10_000, ExpiresAt: now.Add(time.Minute), Scopes: []BudgetScope{scope}})
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrBudgetExceeded) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if accepted.Load() != 100 {
		t.Fatalf("accepted=%d, want 100", accepted.Load())
	}
}

func TestDurableBudgetRestartAndUncertainExpiryRemainCharged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Now().UTC()
	scope := BudgetScope{Kind: "token_day", ID: "token", Period: now.Format("2006-01-02"), LimitMicroUSD: 10_000}
	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	req := BudgetReservation{ID: "crashed", EstimateMicroUSD: 10_000, ExpiresAt: now.Add(-time.Second), Scopes: []BudgetScope{scope}}
	if err = store.ReserveBudget(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	req.ID = "restarted"
	if err = store.ReserveBudget(context.Background(), req); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("restart escaped uncertain reservation: %v", err)
	}
	if err = store.Log(Entry{Timestamp: now, TokenID: "token", ReservationID: "crashed", CostUSD: 0}); err != nil {
		t.Fatal(err)
	}
	if err = store.ReserveBudget(context.Background(), req); err != nil {
		t.Fatalf("known cancellation was not refunded: %v", err)
	}
}

func TestDurableBudgetSettlementExactlyOnceAndSharedScopes(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	month := now.Format("2006-01")
	scopes := []BudgetScope{{Kind: "token_month", ID: "t", Period: month, LimitMicroUSD: 20_000}, {Kind: "org_month", ID: "o", Period: month, LimitMicroUSD: 10_000}, {Kind: "project_month", ID: "p", Period: month, LimitMicroUSD: 10_000}}
	request := BudgetReservation{ID: "r1", EstimateMicroUSD: 10_000, ExpiresAt: now.Add(time.Minute), Scopes: scopes}
	if err = store.ReserveBudget(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err = store.ReserveBudget(context.Background(), request); !errors.Is(err, ErrReservationExists) {
		t.Fatalf("duplicate reservation=%v", err)
	}
	entry := Entry{Timestamp: now, RequestID: "trace", ReservationID: "r1", TokenID: "t", OrganizationID: "o", ProjectID: "p", CostUSD: .005}
	for i := 0; i < 3; i++ {
		if err = store.Log(entry); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.Load(Filter{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	request.ID = "r2"
	request.EstimateMicroUSD = 5_000
	if err = store.ReserveBudget(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.ID = "r3"
	request.EstimateMicroUSD = 1
	if err = store.ReserveBudget(context.Background(), request); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("scope budget escaped: %v", err)
	}
}

func TestDurableBudgetMigratesLegacyUsageWithoutLosingSpend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_entries(id INTEGER PRIMARY KEY,timestamp TEXT NOT NULL,request_id TEXT NOT NULL DEFAULT '',token_id TEXT NOT NULL DEFAULT '',token_description TEXT NOT NULL DEFAULT '',requested_mode TEXT NOT NULL DEFAULT '',requested_model TEXT NOT NULL DEFAULT '',actual_provider TEXT NOT NULL DEFAULT '',status INTEGER NOT NULL DEFAULT 0,duration_ms INTEGER NOT NULL DEFAULT 0,prompt_tokens INTEGER NOT NULL DEFAULT 0,completion_tokens INTEGER NOT NULL DEFAULT 0,cost_usd REAL NOT NULL DEFAULT 0,stream INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = db.Exec(`INSERT INTO usage_entries(timestamp,token_id,cost_usd) VALUES(?,?,?)`, now.Format(time.RFC3339Nano), "t", .01)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		store, err := OpenSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		scope := BudgetScope{Kind: "token_month", ID: "t", Period: now.Format("2006-01"), LimitMicroUSD: 10_000}
		err = store.ReserveBudget(context.Background(), BudgetReservation{ID: fmt.Sprint(i), EstimateMicroUSD: 1, ExpiresAt: now.Add(time.Minute), Scopes: []BudgetScope{scope}})
		store.Close()
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("legacy spend lost on migration/retry: %v", err)
		}
	}
}

func TestMicroUSDCentAndFractionRounding(t *testing.T) {
	for _, tc := range []struct {
		usd  float64
		want int64
	}{{.01, 10_000}, {.001, 1000}, {.0000001, 1}, {.0000009999, 1}} {
		if got := MicroUSD(tc.usd); got != tc.want {
			t.Errorf("MicroUSD(%g)=%d want %d", tc.usd, got, tc.want)
		}
	}
}

func TestDurableUnknownBillingUsesLabeledConservativeEstimate(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	scope := BudgetScope{Kind: "token_month", ID: "t", Period: now.Format("2006-01"), LimitMicroUSD: 10_000}
	request := BudgetReservation{ID: "unknown", EstimateMicroUSD: 10_000, ExpiresAt: now.Add(time.Minute), Scopes: []BudgetScope{scope}}
	if err = store.ReserveBudget(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err = store.Log(Entry{Timestamp: now, ReservationID: "unknown", TokenID: "t", UncertainCost: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := store.Load(Filter{})
	if err != nil || len(entries) != 1 || !entries[0].EstimatedCost || entries[0].CostUSD != .01 {
		t.Fatalf("unknown billing was refunded or unlabeled: %+v err %v", entries, err)
	}
	request.ID = "new"
	if err = store.ReserveBudget(context.Background(), request); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("unknown request regained credit: %v", err)
	}
}

func TestDurableSettlementCannotMoveChargeToAnotherIdentity(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	scope := BudgetScope{Kind: "org_month", ID: "org", Period: now.Format("2006-01"), LimitMicroUSD: 10_000}
	request := BudgetReservation{ID: "r", EstimateMicroUSD: 10_000, ExpiresAt: now.Add(time.Minute), Scopes: []BudgetScope{scope}}
	if err = store.ReserveBudget(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err = store.Log(Entry{Timestamp: now, ReservationID: "r", OrganizationID: "other", CostUSD: .01}); err == nil {
		t.Fatal("settlement moved cost to another tenant")
	}
	request.ID = "new"
	if err = store.ReserveBudget(context.Background(), request); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("invalid settlement released reservation: %v", err)
	}
}

func TestFutureUsageSchemaFailsBeforeColumnChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE schema_versions(component TEXT PRIMARY KEY,version INTEGER);INSERT INTO schema_versions VALUES('usage',99);CREATE TABLE usage_entries(id INTEGER PRIMARY KEY,future_data TEXT)`); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenSQLiteStore(path); err == nil {
		store.Close()
		t.Fatal("future schema accepted")
	}
	columns, err := serverdb.ExistingColumns(db, "usage_entries")
	if err != nil {
		t.Fatal(err)
	}
	if len(columns) != 2 {
		t.Fatalf("future schema was modified before rejecting: %v", columns)
	}
}
