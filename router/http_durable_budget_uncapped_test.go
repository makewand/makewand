package router

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverdb"
	"github.com/makewand/makewand/serverusage"
)

// repeatingUsageLogger delivers every usage callback twice, as a caller that
// retries after an ambiguous commit does.
type repeatingUsageLogger struct {
	*serverusage.SQLiteStore
	calls atomic.Int64
}

func (l *repeatingUsageLogger) Log(entry serverusage.Entry) error {
	for range 2 {
		l.calls.Add(1)
		if err := l.SQLiteStore.Log(entry); err != nil {
			return err
		}
	}
	return nil
}

// A request no budget cap applies to still takes a durable reservation: its ID
// is what makes a repeated usage callback settle once. Without it, the repeat
// inserted a second usage row and billed the cost again, and a cap added
// later would seed from that doubled history.
func TestHTTPDurableBudgetUncappedRequestSettlesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := serverusage.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := serverdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	auth, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{
		{ID: "free", Token: "free-secret", Scopes: []string{serverauth.ScopeChatInvoke}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ok := &stubProvider{name: "custom", available: true, response: "fine", cost: .02}
	lost := &billedFailureProvider{stubProvider: stubProvider{name: "lost", available: true}, failure: &execution.UnknownOutcomeError{Err: errors.New("response lost")}}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
		"custom": {Provider: ok, Access: AccessAPI},
		"lost":   {Provider: lost, Access: AccessAPI},
	}, DefaultModel: "custom"})
	logger := &repeatingUsageLogger{SQLiteStore: store}
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: auth, UsageLogger: logger, UsageReader: store, BudgetReserver: store, BudgetReservationUSD: .01, StrictAccounting: true})
	for _, tc := range []struct {
		model  string
		status int
		cost   float64
	}{
		{"custom", http.StatusOK, .02},
		// An uncertain outcome settles at the reservation estimate.
		{"lost", http.StatusServiceUnavailable, .01},
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer free-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request.WithContext(context.Background()))
		if response.Code != tc.status {
			t.Fatalf("%s: status = %d, want %d: %s", tc.model, response.Code, tc.status, response.Body)
		}
		entries, err := store.Load(serverusage.Filter{Provider: tc.model})
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s: a repeated usage callback left %d usage rows (err %v), want 1", tc.model, len(entries), err)
		}
		if entries[0].CostUSD != tc.cost {
			t.Fatalf("%s: cost = %v, want %v", tc.model, entries[0].CostUSD, tc.cost)
		}
	}
	if got := logger.calls.Load(); got != 4 {
		t.Fatalf("usage callbacks = %d, want each of 2 requests delivered twice", got)
	}
	var rows int
	var billed int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_micro_usd),0) FROM usage_entries WHERE token_id='free'`).Scan(&rows, &billed); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || billed != 30_000 {
		t.Fatalf("usage ledger: %d rows billing %d micro-USD, want 2 rows and 30000", rows, billed)
	}
	var settled int
	if err := db.QueryRow(`SELECT COUNT(*) FROM budget_reservations WHERE state='settled'`).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != 2 {
		t.Fatalf("settled reservations = %d, want one per request", settled)
	}
}
