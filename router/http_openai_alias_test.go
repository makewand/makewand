package router

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/serverauth"
)

// The OpenAI transport never sends a "-cli" placeholder upstream: routing
// requests the codex table's "codex-cli" when the codex route is served by the
// API (no codex CLI installed), and that call must use the configured model.
func TestOpenAINeverSendsACLIPlaceholderModel(t *testing.T) {
	upstream := newBillingUpstream(t, 10, 1, false)
	p := NewOpenAIWithBaseURL("k", "gpt-4o-mini", upstream.URL+"/v1")
	ctx := ContextWithModel(context.Background(), "codex-cli")
	messages := []Message{{Role: "user", Content: "explain lorem"}}
	if _, usage, err := p.Chat(ctx, messages, "", 16); err != nil || usage.Model != "gpt-4o-mini" || usage.Cost <= 0 {
		t.Fatalf("Chat usage = %+v, err = %v; want a priced gpt-4o-mini call", usage, err)
	}
	stream, err := p.ChatStream(ctx, messages, "", 16)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatal(chunk.Error)
		}
	}
	if models, _ := upstream.requests(); len(models) != 2 || models[0] != "gpt-4o-mini" || models[1] != "gpt-4o-mini" {
		t.Fatalf("upstream models = %q, want gpt-4o-mini for both calls", models)
	}
	for requested, want := range map[string]string{"codex-cli": "gpt-4o-mini", "": "gpt-4o-mini", "o1": "o1"} {
		if got := p.ReportedModelID(requested); got != want {
			t.Errorf("ReportedModelID(%q) = %q, want %q", requested, got, want)
		}
	}
}

// OpenAI model names resolve against the deployment's providers, not a
// token's allowlist. Where the OpenAI API serves them (no "codex" registered),
// a token needs the "openai" grant to send them and sees them listed only then.
func TestHTTPOpenAIModelNamesFollowTheOpenAIProviderGrant(t *testing.T) {
	upstream := newBillingUpstream(t, 10, 1, false)
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
		"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-4o", upstream.URL+"/v1"), Access: AccessAPI},
		"claude": {Provider: &stubProvider{name: "claude", available: true, response: "claude answer"}, Access: AccessSubscription},
	}})
	scopes := []string{serverauth.ScopeChatInvoke, serverauth.ScopeModelsRead}
	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{
		{ID: "codex", Token: "codex-only", Scopes: scopes, AllowedProviders: []string{"codex"}},
		{ID: "openai", Token: "openai-only", Scopes: scopes, AllowedProviders: []string{"openai"}},
		{ID: "claude", Token: "claude-only", Scopes: scopes, AllowedProviders: []string{"claude"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"explain what a monad is"}]}`
	if rec := postFix16Chat(t, h, "codex-only", body); rec.Code != http.StatusForbidden {
		t.Fatalf("codex-only token: status %d body %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := postFix16Chat(t, h, "openai-only", body); rec.Code != http.StatusOK {
		t.Fatalf("openai token: status %d body %s", rec.Code, rec.Body.String())
	}
	if models, _ := upstream.requests(); len(models) != 1 || models[0] != "gpt-4o-mini" {
		t.Fatalf("upstream models = %q, want [gpt-4o-mini]", models)
	}
	listed := func(token string) []string {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("%s: %v (%s)", token, err, rec.Body.String())
		}
		var ids []string
		for _, entry := range list.Data {
			ids = append(ids, entry.ID)
		}
		return ids
	}
	if ids := listed("openai-only"); !slices.Contains(ids, "openai") || !slices.Contains(ids, "gpt-4o-mini") || !slices.Contains(ids, "o1") {
		t.Fatalf("openai token lists %q", ids)
	}
	if ids := listed("claude-only"); !slices.Equal(ids, []string{"claude"}) {
		t.Fatalf("claude-only token lists %q, want only claude", ids)
	}
}

// A request naming another OpenAI model runs on a per-request view: its price
// sizes that request's scaled reservation, and the live router keeps calling
// its configured model.
func TestHTTPNamedOpenAIModelIsBoundToTheRequestOnly(t *testing.T) {
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
		"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-4o-mini", "http://127.0.0.1:1/v1"), Access: AccessAPI},
	}})
	prepared, err := r.prepareHTTPChat(httpChatRequest{Model: "O1", Messages: []httpMessage{{Role: "user", Content: "explain lorem"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Router == r || prepared.RequestedModel != "openai" {
		t.Fatalf("prepared %q on the live router=%t, want openai on a request view", prepared.RequestedModel, prepared.Router == r)
	}
	if got, _ := prepared.Router.httpPriceCeiling(); got != tablePrice(t, "o1") {
		t.Fatalf("request ceiling = %+v, want o1's price", got)
	}
	if got, _ := r.httpPriceCeiling(); got != tablePrice(t, "gpt-4o-mini") {
		t.Fatalf("live ceiling = %+v, want gpt-4o-mini's price", got)
	}
	if live, _ := r.getProvider("openai"); reportedModelID(live, "") != "gpt-4o-mini" {
		t.Fatalf("live openai provider calls %q", reportedModelID(live, ""))
	}
	// The configured model needs no price of its own, as for any request.
	unpriced := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
		"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-unpriced-fixture", "http://127.0.0.1:1/v1"), Access: AccessAPI},
	}})
	if _, err := unpriced.prepareHTTPChat(httpChatRequest{Model: "gpt-unpriced-fixture", Messages: []httpMessage{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("configured unpriced model refused: %v", err)
	}
}

// namedModelUpstream is a loopback fake of the OpenAI chat-completions API: it
// answers each model in rejects with that HTTP status, and any other model with
// a short reply, streamed or not.
type namedModelUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	models  []string
	rejects map[string]int
}

func newNamedModelUpstream(t *testing.T, rejects map[string]int) *namedModelUpstream {
	t.Helper()
	u := &namedModelUpstream{rejects: rejects}
	u.Server = httptest.NewServer(u)
	t.Cleanup(u.Close)
	return u
}

func (u *namedModelUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.models = append(u.models, req.Model)
	status := u.rejects[req.Model]
	u.mu.Unlock()
	switch {
	case status != 0:
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":"rejected %s"}}`, req.Model)
	case req.Stream:
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	default:
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":2000}}`)
	}
}

func (u *namedModelUpstream) take() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	models := u.models
	u.models = nil
	return models
}

func namedModelRouter(upstream *namedModelUpstream, quota QuotaController) *Router {
	return mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-4o", upstream.URL+"/v1"), Access: AccessAPI}},
		Quota:     quota,
	})
}

func namedModelChat(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"explain what a monad is"}]}`, model, stream)
}

// A named model's failures are recorded under its own key: they open its own
// circuit and count as its own failures, and leave the openai route, which
// requests without a model and for "openai" use, closed and unblamed.
func TestHTTPNamedOpenAIModelRecordsItsOwnOutcomes(t *testing.T) {
	upstream := newNamedModelUpstream(t, map[string]int{"o1": http.StatusNotFound})
	r := namedModelRouter(upstream, NewQuotaSnapshotter(time.Hour))
	h := r.HTTPHandler(HTTPHandlerOptions{})
	for range defaultCircuitFailureThreshold {
		if rec := postFix16Chat(t, h, "", namedModelChat("o1", false)); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("o1: status %d body %s, want 503", rec.Code, rec.Body.String())
		}
	}
	if open, _ := r.breaker.PeekOpen("openai:o1"); !open {
		t.Fatal("o1's failures did not open its own circuit")
	}
	if open, _ := r.breaker.PeekOpen("openai"); open {
		t.Fatal("o1's failures opened the openai circuit")
	}
	if got := r.usage.FailureCount("openai:o1"); got != defaultCircuitFailureThreshold {
		t.Fatalf("o1 failures = %d, want %d", got, defaultCircuitFailureThreshold)
	}
	for _, name := range []string{"openai", "gpt-4o-mini"} {
		if rec := postFix16Chat(t, h, "", namedModelChat(name, false)); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %s", name, rec.Code, rec.Body.String())
		}
	}
	if failures, served := r.usage.FailureCount("openai"), r.usage.Count("openai"); failures != 0 || served != 1 {
		t.Fatalf("openai route: %d failures and %d successes, want 0 and 1", failures, served)
	}
	if served := r.usage.Count("openai:gpt-4o-mini"); served != 1 {
		t.Fatalf("gpt-4o-mini successes = %d, want 1", served)
	}
	if models := upstream.take(); !slices.Equal(models, []string{"o1", "o1", "o1", "gpt-4o", "gpt-4o-mini"}) {
		t.Fatalf("upstream models = %q", models)
	}
}

// A named model's calls honor the openai route's rate-limit block and open
// circuit, so a provider-wide failure seen by the route stops them too, but
// never take the route's half-open probe: only the route's own calls, which
// still fail here, may close its circuit.
func TestHTTPNamedOpenAIModelHonorsTheOpenAIRoute(t *testing.T) {
	upstream := newNamedModelUpstream(t, nil)
	quota := NewQuotaSnapshotter(time.Hour)
	r := namedModelRouter(upstream, quota)
	quota.MarkExhausted("openai", time.Now().Add(time.Hour))
	for _, stream := range []bool{false, true} {
		if rec := postFix16Chat(t, r.HTTPHandler(HTTPHandlerOptions{}), "", namedModelChat("o1", stream)); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("o1 stream=%t under openai's rate-limit block: status %d body %s", stream, rec.Code, rec.Body.String())
		}
	}
	if models := upstream.take(); len(models) != 0 {
		t.Fatalf("o1 reached the upstream under openai's rate-limit block: %q", models)
	}

	upstream = newNamedModelUpstream(t, map[string]int{"gpt-4o": http.StatusServiceUnavailable})
	r = namedModelRouter(upstream, nil)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	r.breaker.now = func() time.Time { return time.Unix(0, clock.Load()) }
	h := r.HTTPHandler(HTTPHandlerOptions{})
	for range defaultCircuitFailureThreshold {
		postFix16Chat(t, h, "", namedModelChat("openai", false))
	}
	for _, stream := range []bool{false, true} {
		if rec := postFix16Chat(t, h, "", namedModelChat("o1", stream)); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("o1 stream=%t with openai's circuit open: status %d body %s", stream, rec.Code, rec.Body.String())
		}
	}
	if models := upstream.take(); len(models) != defaultCircuitFailureThreshold || slices.Contains(models, "o1") {
		t.Fatalf("upstream models = %q, want only openai's failing calls", models)
	}
	clock.Add(int64(defaultCircuitCooldown + time.Second))
	if rec := postFix16Chat(t, h, "", namedModelChat("o1", false)); rec.Code != http.StatusOK {
		t.Fatalf("o1 after openai's cooldown: status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := postFix16Chat(t, h, "", namedModelChat("openai", false)); rec.Code == http.StatusOK {
		t.Fatalf("openai was served by a failing upstream: %s", rec.Body.String())
	}
	if open, _ := r.breaker.PeekOpen("openai"); !open {
		t.Fatal("openai's circuit closed although its own probe failed")
	}
}

// A routing.json model table for "openai" names the models the openai route
// calls. A request naming a model still calls, reports and bills that model.
func TestHTTPNamedOpenAIModelIsCalledDespiteAnOpenAIModelTable(t *testing.T) {
	dir := t.TempDir()
	table := `{"models":{"openai":{"cheap":"gpt-4o-mini","mid":"gpt-4o-mini","premium":"gpt-4o-mini"}}}`
	if err := os.WriteFile(filepath.Join(dir, "routing.json"), []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := newNamedModelUpstream(t, nil)
	r := namedModelRouter(upstream, nil)
	if err := r.LoadUserOverrides(dir); err != nil {
		t.Fatal(err)
	}
	usage := &usageRecorder{}
	h := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: usage})
	for _, name := range []string{"o1", "gpt-4o"} {
		for _, stream := range []bool{false, true} {
			rec := postFix16Chat(t, h, "", namedModelChat(name, stream))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s stream=%t: status %d body %s", name, stream, rec.Code, rec.Body.String())
			}
			if models := upstream.take(); !slices.Equal(models, []string{name}) {
				t.Fatalf("%s stream=%t: upstream models %q", name, stream, models)
			}
			if !strings.Contains(rec.Body.String(), `"model":"`+name+`"`) {
				t.Fatalf("%s stream=%t: response does not name the model: %s", name, stream, rec.Body.String())
			}
			entry := usage.entries[len(usage.entries)-1]
			if price := priceCompletion(r.routingTables(), name, entry.PromptTokens, entry.CompletionTokens); entry.CostUSD <= 0 || math.Abs(entry.CostUSD-price) > 1e-12 {
				t.Fatalf("%s stream=%t: billed $%v for %d+%d tokens, want $%v", name, stream, entry.CostUSD, entry.PromptTokens, entry.CompletionTokens, price)
			}
		}
	}
	if rec := postFix16Chat(t, h, "", namedModelChat("openai", false)); rec.Code != http.StatusOK {
		t.Fatalf("openai: status %d body %s", rec.Code, rec.Body.String())
	}
	if models := upstream.take(); !slices.Equal(models, []string{"gpt-4o-mini"}) {
		t.Fatalf("openai called %q, want the table's gpt-4o-mini", models)
	}
}
