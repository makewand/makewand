package router

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverdb"
	"github.com/makewand/makewand/serverusage"
)

// go-server-1021-5: with the default durable accounting, a request no budget
// cap applies to takes no reservation: no admission write lock, no row left
// behind. A capped request still reserves, and an uncapped request's
// uncertain outcome still settles at the reservation estimate.
func TestHTTPDurableBudgetReservesOnlyCappedRequests(t *testing.T) {
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
	reservations := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM budget_reservations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	auth, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{
		{ID: "free", Token: "free-secret", Scopes: []string{serverauth.ScopeChatInvoke}},
		{ID: "capped", Token: "capped-secret", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerMonth: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ok := &stubProvider{name: "custom", available: true, response: "fine"}
	lost := &billedFailureProvider{stubProvider: stubProvider{name: "lost", available: true}, failure: &execution.UnknownOutcomeError{Err: errors.New("response lost")}}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
		"custom": {Provider: ok, Access: AccessAPI},
		"lost":   {Provider: lost, Access: AccessAPI},
	}, DefaultModel: "custom"})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: auth, UsageLogger: store, UsageReader: store, BudgetReserver: store, BudgetReservationUSD: .01, StrictAccounting: true})
	invoke := func(token, model string) int {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request.WithContext(context.Background()))
		return response.Code
	}
	for range 3 {
		if status := invoke("free-secret", "custom"); status != http.StatusOK {
			t.Fatalf("uncapped request status = %d", status)
		}
	}
	if got := reservations(); got != 0 {
		t.Fatalf("uncapped requests left %d reservation rows, want none", got)
	}
	if status := invoke("capped-secret", "custom"); status != http.StatusOK {
		t.Fatalf("capped request status = %d", status)
	}
	if got := reservations(); got != 1 {
		t.Fatalf("capped request left %d reservation rows, want its own", got)
	}
	if status := invoke("free-secret", "lost"); status != http.StatusServiceUnavailable {
		t.Fatalf("lost response status = %d", status)
	}
	entries, err := store.Load(serverusage.Filter{Provider: "lost"})
	if err != nil || len(entries) != 1 || !entries[0].EstimatedCost || entries[0].CostUSD != .01 {
		t.Fatalf("uncapped uncertain outcome: entries=%+v err=%v, want the $0.01 estimate", entries, err)
	}
	if got := reservations(); got != 1 {
		t.Fatalf("reservation rows = %d, want only the capped request's", got)
	}
}
