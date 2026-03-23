package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

type auditRecorder struct {
	events []serveraudit.Event
}

func (r *auditRecorder) Log(event serveraudit.Event) {
	r.events = append(r.events, event)
}

type usageRecorder struct {
	entries []serverusage.Entry
}

func (r *usageRecorder) Log(entry serverusage.Entry) {
	r.entries = append(r.entries, entry)
}

func TestHTTPHandler_ChatCompletions(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp httpChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "hello from http" {
		t.Fatalf("content = %q, want %q", resp.Choices[0].Message.Content, "hello from http")
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want %q", resp.Choices[0].FinishReason, "stop")
	}
}

func TestHTTPHandler_ChatCompletionsStream(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, `"object":"chat.completion.chunk"`) {
		t.Fatalf("stream body missing chunk object: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"content":"hello from http"`) {
		t.Fatalf("stream body missing content: %s", bodyText)
	}
	if !strings.Contains(bodyText, "data: [DONE]") {
		t.Fatalf("stream body missing [DONE]: %s", bodyText)
	}
}

func TestHTTPHandler_ModelOverrideUsesRequestedProvider(t *testing.T) {
	claude := &stubProvider{name: "claude", available: true, response: "hello from claude"}
	gemini := &stubProvider{name: "gemini", available: true, response: "hello from gemini"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: claude, Access: AccessSubscription},
			"gemini": {Provider: gemini, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{"model":"gemini","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp httpChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello from gemini" {
		t.Fatalf("content = %q, want %q", resp.Choices[0].Message.Content, "hello from gemini")
	}
}

func TestHTTPHandler_ModeOverrideUsesRequestedMode(t *testing.T) {
	claude := &stubProvider{name: "claude", available: true, response: "hello from claude"}
	gemini := &stubProvider{name: "gemini", available: true, response: "hello from gemini"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: claude, Access: AccessSubscription},
			"gemini": {Provider: gemini, Access: AccessSubscription},
		},
		UsageMode: "fast",
	})

	handler := r.HTTPHandler()
	body := `{"mode":"balanced","messages":[{"role":"user","content":"write a python function"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp httpChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello from claude" {
		t.Fatalf("content = %q, want %q", resp.Choices[0].Message.Content, "hello from claude")
	}
}

func TestHTTPHandler_EmptyMessages(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler()
	body := `{"model":"test","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHTTPHandler_UnknownModelRejected(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler()
	body := `{"model":"unknown","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandler_IgnoresUnsupportedMaxTokens(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler()
	body := `{"messages":[{"role":"user","content":"hi"}],"max_tokens":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// max_tokens is silently ignored, so the request proceeds to routing
	// (which fails with 503 because no provider is configured — not 400).
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("max_tokens should be silently ignored, got 400: %s", rec.Body.String())
	}
}

func TestHTTPHandler_IgnoresUnsupportedTemperature(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler()
	body := `{"messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// temperature is silently ignored, so the request proceeds to routing.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("temperature should be silently ignored, got 400: %s", rec.Body.String())
	}
}

func TestHTTPHandler_RejectsUnknownMode(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler()
	body := `{"mode":"turbo","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandler_PersistsRoutingStatsWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{StatsDir: dir})
	body := `{"messages":[{"role":"user","content":"write a helper function"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	statsPath := filepath.Join(dir, statsFile)
	data, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatalf("read stats file: %v", err)
	}
	if !strings.Contains(string(data), `"claude": 1`) {
		t.Fatalf("stats file = %s, want claude usage persisted", string(data))
	}
}

func TestHTTPHandler_ListModels(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
	})

	handler := r.HTTPHandler()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "claude" {
		t.Fatalf("models = %+v, want [claude]", resp.Data)
	}
}

func TestHTTPHandler_Health(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHTTPHandler_BearerAuth_Rejects(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler(HTTPHandlerOptions{BearerToken: "secret123"})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHTTPHandler_BearerAuth_Accepts(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{BearerToken: "secret123"})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHTTPHandler_BearerAuth_HealthBypassesAuth(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler(HTTPHandlerOptions{BearerToken: "secret123"})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health should bypass auth, got status %d", rec.Code)
	}
}

func TestHTTPHandler_AuthorizerRejectsMissingScope(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{Token: "secret123", Scopes: []string{serverauth.ScopeChatInvoke}},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	r := NewRouterFromConfig(RouterConfig{})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHTTPHandler_AuthorizerFiltersModelList(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:            "secret123",
				Scopes:           []string{serverauth.ScopeModelsRead},
				AllowedProviders: []string{"codex"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	claude := &stubProvider{name: "claude", available: true}
	codex := &stubProvider{name: "codex", available: true}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: claude, Access: AccessSubscription},
			"codex":  {Provider: codex, Access: AccessSubscription},
		},
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "codex" {
		t.Fatalf("models = %+v, want [codex]", resp.Data)
	}
}

func TestHTTPHandler_AuthorizerRejectsDisallowedMode(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:        "secret123",
				Scopes:       []string{serverauth.ScopeChatInvoke},
				AllowedModes: []string{"fast"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandler_AuthorizerRejectsDisallowedProviderOverride(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:            "secret123",
				Scopes:           []string{serverauth.ScopeChatInvoke},
				AllowedProviders: []string{"claude"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	claude := &stubProvider{name: "claude", available: true, response: "hello from claude"}
	gemini := &stubProvider{name: "gemini", available: true, response: "hello from gemini"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: claude, Access: AccessSubscription},
			"gemini": {Provider: gemini, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})
	body := `{"model":"gemini","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandler_AuthorizerFiltersAdaptiveProviders(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:            "secret123",
				Scopes:           []string{serverauth.ScopeChatInvoke},
				AllowedProviders: []string{"claude"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	claude := &stubProvider{name: "claude", available: true, response: "hello from claude"}
	gemini := &stubProvider{name: "gemini", available: true, response: "hello from gemini"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: claude, Access: AccessSubscription},
			"gemini": {Provider: gemini, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})
	body := `{"messages":[{"role":"user","content":"write a helper function"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp httpChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello from claude" {
		t.Fatalf("content = %q, want %q", resp.Choices[0].Message.Content, "hello from claude")
	}
}

func TestHTTPHandler_AuditLogsSuccessfulChat(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:           "runner",
				Token:        "secret123",
				Scopes:       []string{serverauth.ScopeChatInvoke},
				AllowedModes: []string{"balanced"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	recorder := &auditRecorder{}
	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
		UsageMode:    "balanced",
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, AuditLogger: recorder})
	body := `{"messages":[{"role":"user","content":"write a helper function"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(recorder.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Kind != "chat" {
		t.Fatalf("Kind = %q, want %q", event.Kind, "chat")
	}
	if event.TokenID != "runner" {
		t.Fatalf("TokenID = %q, want %q", event.TokenID, "runner")
	}
	if event.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", event.Status)
	}
	if event.ActualProvider != "claude" {
		t.Fatalf("ActualProvider = %q, want %q", event.ActualProvider, "claude")
	}
	if event.Timestamp.IsZero() {
		t.Fatal("Timestamp = zero, want populated audit timestamp")
	}
}

func TestHTTPHandler_LogsUsageEntriesWithRequestID(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:           "runner",
				Token:        "secret123",
				Scopes:       []string{serverauth.ScopeChatInvoke},
				AllowedModes: []string{"balanced"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	recorder := &usageRecorder{}
	stub := &stubProvider{
		name:         "claude",
		available:    true,
		response:     "hello from http",
		inputTokens:  11,
		outputTokens: 7,
		cost:         0.42,
	}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
		UsageMode:    "balanced",
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, UsageLogger: recorder})
	body := `{"messages":[{"role":"user","content":"write a helper function"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "req_custom")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(recorder.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(recorder.entries))
	}
	entry := recorder.entries[0]
	if entry.RequestID != "req_custom" {
		t.Fatalf("RequestID = %q, want %q", entry.RequestID, "req_custom")
	}
	if entry.TokenID != "runner" {
		t.Fatalf("TokenID = %q, want %q", entry.TokenID, "runner")
	}
	if entry.ActualProvider != "claude" {
		t.Fatalf("ActualProvider = %q, want %q", entry.ActualProvider, "claude")
	}
	if entry.CostUSD != 0.42 {
		t.Fatalf("CostUSD = %.2f, want 0.42", entry.CostUSD)
	}
	if entry.PromptTokens != 11 || entry.CompletionTokens != 7 {
		t.Fatalf("token counts = %d/%d, want 11/7", entry.PromptTokens, entry.CompletionTokens)
	}
}

func TestHTTPHandler_AuditLogsForbiddenModelsScope(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "viewer",
				Token:  "secret123",
				Scopes: []string{serverauth.ScopeChatInvoke},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	recorder := &auditRecorder{}
	r := NewRouterFromConfig(RouterConfig{})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, AuditLogger: recorder})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Kind != "models" {
		t.Fatalf("Kind = %q, want %q", event.Kind, "models")
	}
	if event.Scope != serverauth.ScopeModelsRead {
		t.Fatalf("Scope = %q, want %q", event.Scope, serverauth.ScopeModelsRead)
	}
	if event.Status != http.StatusForbidden {
		t.Fatalf("Status = %d, want 403", event.Status)
	}
}

func TestHTTPHandler_RejectsRequestsOverHourlyQuota(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:              "secret123",
				Scopes:             []string{serverauth.ScopeModelsRead},
				MaxRequestsPerHour: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	r := NewRouterFromConfig(RouterConfig{})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})

	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req1.Header.Set("Authorization", "Bearer secret123")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer secret123")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body: %s", rec2.Code, rec2.Body.String())
	}
}

func TestHTTPHandler_RejectsRequestsOverDailyQuota(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:             "secret123",
				Scopes:            []string{serverauth.ScopeModelsRead},
				MaxRequestsPerDay: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	r := NewRouterFromConfig(RouterConfig{})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})

	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req1.Header.Set("Authorization", "Bearer secret123")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer secret123")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "max_requests_per_day") {
		t.Fatalf("second body = %q, want daily quota error", rec2.Body.String())
	}
}

func TestHTTPHandler_RejectsRequestsOverDailyCostBudget(t *testing.T) {
	stub := &stubProvider{
		name:         "claude",
		available:    true,
		response:     "hello from http",
		inputTokens:  10,
		outputTokens: 5,
		cost:         1.0,
	}
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:            "secret123",
				Scopes:           []string{serverauth.ScopeChatInvoke},
				MaxCostUSDPerDay: 1.0,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req1.Header.Set("Authorization", "Bearer secret123")
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body: %s", rec1.Code, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req2.Header.Set("Authorization", "Bearer secret123")
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "max_cost_usd_per_day") {
		t.Fatalf("second body = %q, want cost budget error", rec2.Body.String())
	}
}

func TestHTTPHandler_MethodNotAllowed(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})

	handler := r.HTTPHandler()
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
