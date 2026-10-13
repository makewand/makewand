package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

// fakeOpenAIUpstream answers OpenAI chat completions. Streams carry no usage
// (like most OpenAI-compatible gateways); non-streaming replies report usage.
func fakeOpenAIUpstream(t *testing.T, words int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if stream, _ := payload["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < words; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"word%d \"}}]}\n\n", i)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":1000,"completion_tokens":2000}}`, strings.Repeat("word ", words))
	}))
	t.Cleanup(server.Close)
	return server
}

// unpricedOpenAIRouter routes to the real OpenAI provider configured with a
// model the pricing table does not know, as an API-access provider.
func unpricedOpenAIRouter(t *testing.T, baseURL string) *Router {
	t.Helper()
	provider := NewOpenAIWithBaseURL("fixture-key", "gpt-unpriced-fixture", baseURL+"/v1")
	return mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"openai": {Provider: provider, Access: AccessAPI}},
		DefaultModel: "openai",
		CodingModel:  "openai",
	})
}

func postFix16Chat(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A stream through an API provider whose model has no pricing entry must be
// billed at the same conservative fallback rate as the non-streaming call,
// not at $0.
func TestHTTPStreamThroughUnpricedAPIModelIsBilledAtFallback_Fix16(t *testing.T) {
	upstream := fakeOpenAIUpstream(t, 1500)
	r := unpricedOpenAIRouter(t, upstream.URL)
	store, err := serverusage.OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{ID: "t", Token: "secret", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerMonth: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	h := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, UsageLogger: store, UsageReader: store})
	prompt := strings.Repeat("lorem ", 1500)
	for _, stream := range []bool{false, true} {
		rec := postFix16Chat(t, h, "secret", fmt.Sprintf(`{"model":"openai","stream":%t,"messages":[{"role":"user","content":%q}]}`, stream, prompt))
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%t status=%d body=%s", stream, rec.Code, rec.Body.String())
		}
	}
	entries, err := store.Load(serverusage.Filter{TokenID: "t"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	var streamed serverusage.Entry
	for _, e := range entries {
		if e.CompletionTokens != 2000 { // the non-streaming entry reports upstream usage
			streamed = e
		}
	}
	if streamed.CostUSD <= 0 {
		t.Fatalf("streamed request billed $%v for %d prompt + %d completion tokens; want the unpriced fallback rate", streamed.CostUSD, streamed.PromptTokens, streamed.CompletionTokens)
	}
	want := (float64(streamed.PromptTokens)*conservativeInputCostPer1M + float64(streamed.CompletionTokens)*conservativeOutputCostPer1M) / 1_000_000
	if math.Abs(streamed.CostUSD-want) > 1e-6 {
		t.Fatalf("streamed cost = %v, want fallback-priced %v", streamed.CostUSD, want)
	}
}

func TestEstimateUsageForRouteUsesUnpricedFallbackAndStaysEstimated_Fix16(t *testing.T) {
	r := unpricedOpenAIRouter(t, "http://127.0.0.1:1")
	messages := []Message{{Role: "user", Content: strings.Repeat("lorem ", 400)}}
	output := strings.Repeat("word ", 800)
	for _, modelID := range []string{"", "gpt-unpriced-fixture"} {
		usage := r.EstimateUsageForRoute(RouteResult{Actual: "openai", ModelID: modelID}, messages, "sys", output)
		want := (float64(usage.InputTokens)*conservativeInputCostPer1M + float64(usage.OutputTokens)*conservativeOutputCostPer1M) / 1_000_000
		if usage.Cost <= 0 || math.Abs(usage.Cost-want) > 1e-12 {
			t.Fatalf("model %q: estimated cost = %v, want fallback %v", modelID, usage.Cost, want)
		}
		if usage.MeasuredCost || usage.MeasuredTokens {
			t.Fatalf("model %q: heuristic estimate marked measured: %+v", modelID, usage)
		}
	}
	// A priced model keeps its table price.
	priced := r.EstimateUsageForRoute(RouteResult{Actual: "openai", ModelID: "gpt-4o"}, messages, "sys", output)
	entry, _ := r.routingTables().costFor("gpt-4o")
	if want := (float64(priced.InputTokens)*entry.Input + float64(priced.OutputTokens)*entry.Output) / 1_000_000; math.Abs(priced.Cost-want) > 1e-12 {
		t.Fatalf("priced model cost = %v, want table price %v", priced.Cost, want)
	}
	// Subscription access never gets a dollar estimate.
	sub := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-unpriced-fixture", "http://127.0.0.1:1/v1"), Access: AccessSubscription}}, DefaultModel: "openai"})
	if got := sub.EstimateUsageForRoute(RouteResult{Actual: "openai"}, messages, "sys", output); got.Cost != 0 {
		t.Fatalf("subscription estimate cost = %v, want 0", got.Cost)
	}
}

// The reservation ceiling covers unpriced API models at the fallback rate, so
// a worst-case request through them is reserved (and refused when it cannot
// fit the grant) instead of falling back to the fixed floor.
func TestHTTPReservationCeilingIncludesUnpricedAPIModels_Fix16(t *testing.T) {
	r := unpricedOpenAIRouter(t, "http://127.0.0.1:1")
	price, ok := r.httpPriceCeiling("")
	if !ok || price.Input < conservativeInputCostPer1M || price.Output < conservativeOutputCostPer1M {
		t.Fatalf("price ceiling = %+v ok=%v, want at least the unpriced fallback rate", price, ok)
	}
	if pinned, ok := r.httpPriceCeiling("openai"); !ok || pinned != price {
		t.Fatalf("pinned ceiling = %+v ok=%v, want %+v", pinned, ok, price)
	}
	prepared := &preparedHTTPChat{Router: r, Task: TaskCode, Messages: []Message{{Role: "user", Content: "hi"}}}
	if got := r.httpReservationUSD(0.0001, prepared); got <= 0.0001 {
		t.Fatalf("reservation = %v, want the fallback-priced worst case above the floor", got)
	}

	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{Token: "tok", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerDay: 0.02}}})
	if err != nil {
		t.Fatal(err)
	}
	h := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, BudgetReservationUSD: 0.01, ScaleBudgetReservation: true})
	if rec := postFix16Chat(t, h, "tok", `{"model":"openai","stream":true,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (fallback-priced reservation exceeds the cap); body=%s", rec.Code, reservationTail(rec.Body.String(), 300))
	}
}

// The stream spend guard prices unpriced API models at the fallback rate too,
// so a runaway stream is stopped at the reservation.
func TestHTTPStreamSpendGuardCoversUnpricedAPIModels_Fix16(t *testing.T) {
	prov := &reservationStreamProvider{name: "openai"}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: prov, Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{Token: "tok", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerDay: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	h := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, BudgetReservationUSD: 0.01, ScaleBudgetReservation: true})
	rec := postFix16Chat(t, h, "tok", `{"model":"openai","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(rec.Body.String(), "budget_reservation_exceeded") {
		t.Fatalf("stream was not stopped at the reservation; status=%d body tail=%q", rec.Code, reservationTail(rec.Body.String(), 300))
	}
	if n := prov.sent.Load(); n >= 1000 {
		t.Fatalf("provider streamed all %d chunks; guard did not stop it", n)
	}
}
