package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

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
