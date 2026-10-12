package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

type reservationStreamProvider struct {
	name      string
	cancelled atomic.Bool
	sent      atomic.Int64
}

func (p *reservationStreamProvider) Name() string      { return p.name }
func (p *reservationStreamProvider) IsAvailable() bool { return true }
func (p *reservationStreamProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	return "ok", Usage{Provider: p.name}, nil
}

// ChatStream emits ~2.9k-token chunks until its context is cancelled, modelling
// a provider that ignores the per-call output cap.
func (p *reservationStreamProvider) ChatStream(ctx context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk)
	chunk := strings.Repeat("x", 10_000)
	go func() {
		defer close(ch)
		for i := 0; i < 1000; i++ {
			select {
			case ch <- StreamChunk{Content: chunk}:
				p.sent.Add(1)
			case <-ctx.Done():
				p.cancelled.Store(true)
				return
			}
		}
		select {
		case ch <- StreamChunk{Done: true}:
		case <-ctx.Done():
			p.cancelled.Store(true)
		}
	}()
	return ch, nil
}

func reservationRouter(access AccessType, prov Provider) *Router {
	return mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: prov, Access: access}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestHTTPReservationUSDUsesFloorWhenUnpriced(t *testing.T) {
	r := reservationRouter(AccessSubscription, &reservationStreamProvider{name: "claude"})
	prepared := &preparedHTTPChat{Router: r, Task: TaskCode, Messages: []Message{{Role: "user", Content: "hi"}}}
	if got := r.httpReservationUSD(0.25, prepared); got != 0.25 {
		t.Fatalf("subscription-only reservation = %v, want floor 0.25", got)
	}
	if got := r.httpReservationUSD(0, prepared); got != 0 {
		t.Fatalf("disabled reservation = %v, want 0", got)
	}
	if got := r.httpReservationUSD(math.NaN(), prepared); got != 0 {
		t.Fatalf("NaN floor reservation = %v, want 0", got)
	}
}

func TestHTTPReservationUSDScalesWithMaxTokensPricingAndEnsembleFanout(t *testing.T) {
	r := reservationRouter(AccessAPI, &reservationStreamProvider{name: "claude"})
	price, ok := r.httpPriceCeiling("")
	if !ok {
		t.Fatal("API claude provider has no priced model in defaults")
	}
	msgs := []Message{{Role: "user", Content: strings.Repeat("word ", 2000)}}
	prepared := &preparedHTTPChat{Router: r, Task: TaskCode, Messages: msgs, System: "sys"}
	prompt := float64(estimateTokens(buildCLIPrompt(msgs, "sys")))
	attempt := func(in, out float64) float64 { return (in*price.Input + out*price.Output) / 1_000_000 }

	single := r.httpReservationUSD(0.0001, prepared)
	if want := attempt(prompt, float64(MaxTokensForTask(TaskCode))); !approxEqual(single, want) {
		t.Fatalf("single-attempt reservation = %v, want %v", single, want)
	}
	if got := r.httpReservationUSD(single*10, prepared); got != single*10 {
		t.Fatalf("floor above estimate = %v, want floor %v", got, single*10)
	}

	power := r.cloneWithMode(ModePower)
	phase := taskToBuildPhase(TaskCode)
	pe, ok := power.routingTables().powerEnsembleFor(phase)
	if !ok || len(pe.Generators) == 0 {
		t.Fatalf("no power ensemble for %v", phase)
	}
	gen := float64(len(pe.Generators))
	maxOut := float64(maxTokensForPhase(phase))
	want := gen*attempt(prompt, maxOut) + attempt(prompt+gen*maxOut, maxOut) + attempt(prompt, maxOut)
	got := power.httpReservationUSD(0.0001, &preparedHTTPChat{Router: power, Task: TaskCode, Messages: msgs, System: "sys"})
	if !approxEqual(got, want) {
		t.Fatalf("power reservation = %v, want generators+judge+fallback %v", got, want)
	}
	if got <= single*(gen+1) {
		t.Fatalf("power reservation %v does not scale with fanout (single %v × %v attempts)", got, single, gen+2)
	}
}

// A grant whose daily cap fits the fixed floor but not the priced worst case
// must be rejected up front instead of overshooting after the provider runs.
func TestHTTPReservationRejectsWhenScaledEstimateExceedsTokenBudget(t *testing.T) {
	prov := &reservationStreamProvider{name: "claude"}
	r := reservationRouter(AccessAPI, prov)
	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{
		Token: "tok", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerDay: 0.02,
	}}})
	if err != nil {
		t.Fatalf("authorizer: %v", err)
	}
	h := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, BudgetReservationUSD: 0.01, ScaleBudgetReservation: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (scaled reservation exceeds cap); body=%s", rec.Code, rec.Body.String())
	}
}

// Without ScaleBudgetReservation the fixed floor is reserved and streams are
// never cut by a price estimate: a cap that fits the floor admits the request.
func TestHTTPReservationDefaultsToFixedFloor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		prov := &reservationStreamProvider{name: "claude"}
		r := reservationRouter(AccessAPI, prov)
		authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{
			Token: "tok", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerDay: 0.02,
		}}})
		if err != nil {
			t.Fatalf("authorizer: %v", err)
		}
		h := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, BudgetReservationUSD: 0.01})
		body := fmt.Sprintf(`{"model":"claude","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests || strings.Contains(rec.Body.String(), "budget_reservation_exceeded") {
			t.Fatalf("stream=%t: default reservation was scaled; status=%d body tail=%q", stream, rec.Code, reservationTail(rec.Body.String(), 300))
		}
	}
}

func TestHTTPStreamStopsWhenEstimatedSpendExceedsReservation(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			prov := &reservationStreamProvider{name: "claude"}
			r := reservationRouter(AccessAPI, prov)
			authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{
				Token: "tok", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerDay: 1000,
			}}})
			if err != nil {
				t.Fatalf("authorizer: %v", err)
			}
			h := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, BudgetReservationUSD: 0.01, ScaleBudgetReservation: true})
			body := `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			if path == "/v1/responses" {
				body = `{"model":"claude","stream":true,"input":"hi"}`
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer tok")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), "budget_reservation_exceeded") {
				t.Fatalf("stream was not stopped at the reservation; status=%d body tail=%q", rec.Code, reservationTail(rec.Body.String(), 300))
			}
			if n := prov.sent.Load(); n >= 1000 {
				t.Fatalf("provider streamed all %d chunks; guard did not stop it", n)
			}
			deadline := time.Now().Add(2 * time.Second)
			for !prov.cancelled.Load() {
				if time.Now().After(deadline) {
					t.Fatal("provider stream context was not cancelled after the guard fired")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func reservationTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// The durable reservation is the scaled estimate: a priced unknown-outcome
// request is held (and billed) at the worst-case estimate, not the fixed floor,
// and the durable reserve/settle protocol is otherwise unchanged.
func TestHTTPDurableReservationUsesScaledEstimate(t *testing.T) {
	store, err := serverusage.OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &billedFailureProvider{stubProvider: stubProvider{name: "claude", available: true}, failure: &execution.UnknownOutcomeError{Err: errors.New("response lost")}}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: provider, Access: AccessAPI}}, DefaultModel: "claude"})
	prepared, err := r.prepareHTTPChat(httpChatRequest{Model: "claude", Messages: []httpMessage{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("prepareHTTPChat: %v", err)
	}
	reservation := prepared.Router.httpReservationUSD(.01, prepared)
	if reservation <= .01 {
		t.Fatalf("priced reservation %v did not scale above the floor", reservation)
	}
	auth, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{ID: "t", Token: "secret", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerMonth: reservation * 1.5}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: auth, UsageLogger: store, UsageReader: store, BudgetReserver: store, BudgetReservationUSD: .01, ScaleBudgetReservation: true, StrictAccounting: true})
	invoke := func() int {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	if status := invoke(); status != http.StatusServiceUnavailable {
		t.Fatalf("unknown response status=%d", status)
	}
	entries, err := store.Load(serverusage.Filter{TokenID: "t"})
	if err != nil || len(entries) != 1 || !entries[0].EstimatedCost {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	if got := entries[0].CostUSD; math.Abs(got-reservation) > 1e-6 {
		t.Fatalf("unknown outcome billed %v, want scaled reservation %v", got, reservation)
	}
	// Remaining headroom (0.5 × reservation) cannot admit another worst case.
	if status := invoke(); status != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d, want 429", status)
	}
}
