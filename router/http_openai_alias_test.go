package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// openAIChatLimits holds the output caps of a chat-completions request body.
type openAIChatLimits struct {
	MaxTokens           *int `json:"max_tokens"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
}

// openAIRefusesMaxTokens is the OpenAI API's 400 body for max_tokens sent to
// a reasoning model.
const openAIRefusesMaxTokens = `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`

// limit reads the output cap as the OpenAI API does: o-series and GPT-5
// models refuse max_tokens (refused is then the API's error body) and take
// max_completion_tokens, which every model accepts.
func (l openAIChatLimits) limit(model string) (limit int, refused string) {
	model = strings.ToLower(model)
	reasoning := strings.HasPrefix(model, "gpt-5") || len(model) > 1 && model[0] == 'o' && model[1] >= '0' && model[1] <= '9'
	switch {
	case reasoning && l.MaxTokens != nil:
		return 0, openAIRefusesMaxTokens
	case l.MaxCompletionTokens != nil:
		return *l.MaxCompletionTokens, ""
	case l.MaxTokens != nil:
		return *l.MaxTokens, ""
	}
	return 0, ""
}

// OpenAI's reasoning models refuse max_tokens, so the transport caps their
// output with max_completion_tokens: o1 and o3-mini, which GET /v1/models
// lists, got a 400 from api.openai.com, streamed or not. Every other model,
// including an OpenAI-compatible endpoint's own, keeps max_tokens.
func TestOpenAICapsReasoningModelsWithMaxCompletionTokens(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
			openAIChatLimits
		}
		if err != nil || json.Unmarshal(raw, &body) != nil || json.Unmarshal(raw, &req) != nil {
			http.Error(w, "unreadable request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if _, refused := req.limit(req.Model); refused != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, refused)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`)
	}))
	t.Cleanup(upstream.Close)

	const completion, legacy = "max_completion_tokens", "max_tokens"
	messages := []Message{{Role: "user", Content: "explain lorem"}}
	for model, field := range map[string]string{
		"o1": completion, "o1-mini": completion, "o3": completion, "o3-mini": completion, "o4-mini": completion,
		"gpt-5": completion, "gpt-5-mini": completion, "codex-mini-latest": completion,
		"gpt-4o": legacy, "gpt-4o-mini": legacy, "gpt-4.1": legacy, "deepseek-chat": legacy,
	} {
		p := NewOpenAIWithBaseURL("k", model, upstream.URL+"/v1")
		if _, _, err := p.Chat(context.Background(), messages, "be brief", 123); err != nil {
			t.Fatalf("%s Chat: %v", model, err)
		}
		stream, err := p.ChatStream(context.Background(), messages, "be brief", 123)
		if err != nil {
			t.Fatalf("%s ChatStream: %v", model, err)
		}
		for chunk := range stream {
			if chunk.Error != nil {
				t.Fatalf("%s ChatStream: %v", model, chunk.Error)
			}
		}
		other := map[string]string{completion: legacy, legacy: completion}[field]
		mu.Lock()
		sent := bodies
		bodies = nil
		mu.Unlock()
		if len(sent) != 2 {
			t.Fatalf("%s: %d upstream requests, want 2", model, len(sent))
		}
		for _, body := range sent {
			if string(body[field]) != "123" || body[other] != nil {
				t.Fatalf("%s: upstream got %s=%s and %s=%s, want only %s=123", model, field, body[field], other, body[other], field)
			}
		}
	}
}

// namedModelUpstream is a loopback fake of the OpenAI chat-completions API: it
// answers each model in rejects with that HTTP status, a reasoning model sent
// max_tokens with the API's 400, and any other model with a short reply,
// streamed or not.
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
		openAIChatLimits
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.models = append(u.models, req.Model)
	status := u.rejects[req.Model]
	u.mu.Unlock()
	_, refused := req.limit(req.Model)
	switch {
	case refused != "":
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, refused)
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

// A request refused because its named model's own route is blocked says so,
// to the client and in skip traces: an o1-only 429 was reported as "circuit
// open for openai", an outage of a route that kept serving. A refusal because
// the openai route itself is blocked still names openai.
func TestHTTPNamedOpenAIModelRefusalsNameTheBlockedRoute(t *testing.T) {
	traced := func(r *Router) func() []string {
		var mu sync.Mutex
		var details []string
		r.SetTraceSink(TraceSinkFunc(func(e TraceEvent) {
			if strings.HasPrefix(e.Detail, "circuit open for ") {
				mu.Lock()
				details = append(details, e.Detail)
				mu.Unlock()
			}
		}))
		return func() []string {
			mu.Lock()
			defer mu.Unlock()
			taken := details
			details = nil
			return taken
		}
	}
	for _, tc := range []struct{ status, failures int }{
		{http.StatusNotFound, defaultCircuitFailureThreshold}, // o1's circuit opens
		{http.StatusTooManyRequests, 1},                       // o1 is rate-limit blocked
	} {
		r := namedModelRouter(newNamedModelUpstream(t, map[string]int{"o1": tc.status}), NewQuotaSnapshotter(time.Hour))
		take := traced(r)
		h := r.HTTPHandler(HTTPHandlerOptions{})
		for range tc.failures {
			postFix16Chat(t, h, "", namedModelChat("o1", false))
		}
		take()
		for _, stream := range []bool{false, true} {
			rec := postFix16Chat(t, h, "", namedModelChat("o1", stream))
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "openai:o1") {
				t.Fatalf("upstream %d: o1 stream=%t: status %d body %s, want 503 naming openai:o1", tc.status, stream, rec.Code, rec.Body.String())
			}
		}
		details := take()
		if len(details) == 0 {
			t.Fatalf("upstream %d: no skip traced", tc.status)
		}
		for _, detail := range details {
			if !strings.HasPrefix(detail, "circuit open for openai:o1 ") {
				t.Fatalf("upstream %d: skip detail %q, want o1's own route", tc.status, detail)
			}
		}
	}

	r := namedModelRouter(newNamedModelUpstream(t, map[string]int{"gpt-4o": http.StatusServiceUnavailable}), nil)
	take := traced(r)
	h := r.HTTPHandler(HTTPHandlerOptions{})
	for range defaultCircuitFailureThreshold {
		postFix16Chat(t, h, "", namedModelChat("openai", false))
	}
	take()
	if rec := postFix16Chat(t, h, "", namedModelChat("o1", false)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("o1 with openai's circuit open: status %d body %s", rec.Code, rec.Body.String())
	}
	if details := take(); len(details) == 0 || !strings.HasPrefix(details[0], "circuit open for openai (") {
		t.Fatalf("skip details %q, want openai's own circuit", details)
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
// calls. A request naming a model still calls, reports and bills that model;
// the upstream refuses a reasoning model's call that sends max_tokens.
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
	for _, name := range []string{"o1", "o3-mini", "gpt-4o"} {
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
