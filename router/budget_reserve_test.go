package router

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

func TestBudgetReserverAdmitAndSettle(t *testing.T) {
	b := &budgetReserver{}
	b.seed("s", 0) // ledger says $0 spent this month
	// budget 1.0, estimate 0.5 → two admissions fit, the third does not.
	if !b.admit("s", 1, 0.5) {
		t.Fatal("first admit should succeed")
	}
	if !b.admit("s", 1, 0.5) {
		t.Fatal("second admit should succeed")
	}
	if b.admit("s", 1, 0.5) {
		t.Fatal("third admit should be rejected (0 committed + 1.0 reserved >= 1.0)")
	}
	// Settling one request with zero realized cost frees exactly one slot.
	b.settle("s", 0.5, 0)
	if !b.admit("s", 1, 0.5) {
		t.Fatal("admit should succeed again after a settle with no cost")
	}

	// A scope already at cap by committed (seeded) spend is rejected.
	b.seed("other", 1)
	if b.admit("other", 1, 0) {
		t.Fatal("scope already at cap by committed spend must be rejected")
	}
}

// TestBudgetReserverCommittedPersistsAcrossSettle is the anti-TOCTOU invariant:
// once a request settles with a realized cost, that cost stays counted (in
// committed), so a later request cannot re-consume the headroom the earlier one
// spent — even though the durable ledger is never re-read after seeding.
func TestBudgetReserverCommittedPersistsAcrossSettle(t *testing.T) {
	b := &budgetReserver{}
	b.seed("s", 0) // seeded once from a $0 ledger
	if !b.admit("s", 10, 1) {
		t.Fatal("admit should succeed against fresh budget")
	}
	b.settle("s", 1, 9) // realized $9 → committed 9, reservation released

	// $9 committed + $1 reserved would reach the $10 cap: exactly one more $1
	// request fits, and the next is rejected — the $9 is NOT forgotten.
	if !b.admit("s", 10, 1) {
		t.Fatal("one more request should fit ($9 committed + $1 reserved < $10)")
	}
	if b.admit("s", 10, 1) {
		t.Fatal("next request must be rejected ($9 committed + $2 reserved >= $10)")
	}
}

// TestBudgetReserverProjectedAdmission proves an admitted request counts its own
// reservation toward the cap, so a request whose realized cost is within the
// reservation can never push a near-cap scope over budget.
func TestBudgetReserverProjectedAdmission(t *testing.T) {
	b := &budgetReserver{}
	b.seed("s", 0.99) // $0.99 of a $1 budget already spent
	// A $0.50-reservation request would project to $1.49 > $1 → must be rejected,
	// even though its eventual realized cost might be under $0.50.
	if b.admit("s", 1, 0.5) {
		t.Fatal("admit should reject: 0.99 committed + 0.50 reservation > 1.0 budget")
	}
	// A reservation that fits exactly is admitted.
	if !b.admit("s", 1, 0.01) {
		t.Fatal("admit should accept: 0.99 + 0.01 == 1.0 budget")
	}
}

// TestBudgetReserverSanitizesCosts ensures NaN/Inf/negative values cannot poison
// the authoritative committed total.
func TestBudgetReserverSanitizesCosts(t *testing.T) {
	b := &budgetReserver{}
	b.seed("s", math.Inf(1)) // must be coerced to 0
	if !b.admit("s", 1, 0) {
		t.Fatal("Inf-seeded committed should have been sanitized to 0 (admit ok)")
	}
	b.settle("s", 0, math.NaN()) // must be ignored, not poison committed
	b.settle("s", 0, -5)         // negative cost must not reduce committed
	if !b.admit("s", 1, 0) {
		t.Fatal("committed should still be 0 after NaN/negative settles")
	}
}

// blockingProvider stays inside Chat until released, so a request holds its
// budget reservation for the duration of the test.
type blockingProvider struct {
	name    string
	entered chan struct{}
	release chan struct{}
}

func (p *blockingProvider) Name() string      { return p.name }
func (p *blockingProvider) IsAvailable() bool { return true }

func (p *blockingProvider) Chat(_ context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	p.entered <- struct{}{}
	<-p.release
	return "ok", Usage{Provider: p.name, Model: p.name}, nil
}

func (p *blockingProvider) ChatStream(_ context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: "ok", Done: true}
	close(ch)
	return ch, nil
}

// TestBudgetReservationBoundsConcurrentOvershoot proves the reservation is wired
// into the handler: while two requests are in flight against a $1 org budget with
// a $0.50 reservation, a third concurrent request is rejected instead of being
// admitted on the same (still-zero) ledger reading.
func TestBudgetReservationBoundsConcurrentOvershoot(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	teamStore, err := serverteam.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("team store: %v", err)
	}
	defer teamStore.Close()
	usageStore, err := serverusage.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("usage store: %v", err)
	}
	defer usageStore.Close()

	org, err := teamStore.CreateOrganization(serverteam.Organization{ID: "org1", Name: "Org", MonthlyBudgetUSD: 1})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{{Token: "sec", Scopes: []string{serverauth.ScopeChatInvoke}, OrganizationID: org.ID}},
	})
	if err != nil {
		t.Fatalf("authorizer: %v", err)
	}

	prov := &blockingProvider{name: "claude", entered: make(chan struct{}, 8), release: make(chan struct{})}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: prov, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{
		Authorizer:           authz,
		UsageReader:          usageStore,
		TeamStore:            teamStore,
		BudgetReservationUSD: 0.5,
	})

	fire := func() *httptest.ResponseRecorder {
		body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer sec")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Two concurrent requests that block inside generation, each holding a $0.50
	// reservation (total $1.00 reserved against the $1 budget).
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = fire().Code
		}(i)
	}
	<-prov.entered
	<-prov.entered

	// A third request now sees 0 ledger + $1.00 reserved >= $1 budget → rejected.
	rec := fire()
	if rec.Code != http.StatusTooManyRequests {
		close(prov.release)
		wg.Wait()
		t.Fatalf("third concurrent request: status = %d, want 429; body: %s", rec.Code, rec.Body.String())
	}

	close(prov.release)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("in-flight request %d: status = %d, want 200", i, c)
		}
	}
}
