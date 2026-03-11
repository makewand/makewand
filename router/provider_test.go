package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockTransport redirects all HTTP requests to the provided handler, allowing
// tests to intercept provider API calls without real network access.
type mockTransport struct {
	handler http.HandlerFunc
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	m.handler(w, req)
	return w.Result(), nil
}

func newMockClient(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: &mockTransport{handler: handler}}
}

// --- Claude provider tests ---

func TestClaude_Chat_NormalResponse(t *testing.T) {
	want := "Hello from Claude"

	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Error("request missing x-api-key header")
		}
		resp := claudeResponse{
			Content: []claudeContentBlock{{Text: want}},
			Usage:   claudeUsage{InputTokens: 10, OutputTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	c := NewClaude("test-key", "claude-haiku-4-5-20251001")
	c.chatClient = newMockClient(handler)

	got, usage, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got != want {
		t.Fatalf("Chat() content = %q, want %q", got, want)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("Chat() usage = %+v, want InputTokens=10 OutputTokens=5", usage)
	}
	if usage.Provider != "claude" {
		t.Fatalf("Chat() usage.Provider = %q, want claude", usage.Provider)
	}
}

func TestClaude_Chat_HTTP429(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}

	c := NewClaude("test-key", claudeDefaultModel)
	c.chatClient = newMockClient(handler)

	_, _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err == nil {
		t.Fatal("Chat() error = nil, want error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("Chat() error = %q, want it to mention 429", err.Error())
	}
	assertProviderErrorKind(t, err, ErrorKindRateLimit, http.StatusTooManyRequests)
}

func TestClaude_Chat_NetworkError(t *testing.T) {
	// Transport that returns an error unconditionally.
	c := NewClaude("test-key", claudeDefaultModel)
	c.chatClient = &http.Client{
		Transport: &mockTransport{handler: func(w http.ResponseWriter, r *http.Request) {
			// Simulate a broken connection by closing immediately with a bad status.
			w.WriteHeader(http.StatusInternalServerError)
		}},
	}

	_, _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err == nil {
		t.Fatal("Chat() error = nil, want error for 500 response")
	}
	assertProviderErrorKind(t, err, ErrorKindUnavailable, http.StatusInternalServerError)
}

func TestClaude_IsAvailable(t *testing.T) {
	if NewClaude("", "").IsAvailable() {
		t.Fatal("IsAvailable() = true for empty API key, want false")
	}
	if !NewClaude("key", "").IsAvailable() {
		t.Fatal("IsAvailable() = false for non-empty API key, want true")
	}
}

// --- OpenAI provider tests ---

func TestOpenAI_Chat_NormalResponse(t *testing.T) {
	want := "Hello from OpenAI"

	handler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("request missing Bearer authorization header")
		}
		resp := openaiResponse{}
		resp.Choices = append(resp.Choices, struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}{Message: struct {
			Content string `json:"content"`
		}{Content: want}})
		resp.Usage.PromptTokens = 8
		resp.Usage.CompletionTokens = 6
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	o := NewOpenAI("test-key", "gpt-4o-mini")
	o.chatClient = newMockClient(handler)

	got, usage, err := o.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got != want {
		t.Fatalf("Chat() content = %q, want %q", got, want)
	}
	if usage.InputTokens != 8 || usage.OutputTokens != 6 {
		t.Fatalf("Chat() usage = %+v, want InputTokens=8 OutputTokens=6", usage)
	}
	if usage.Provider != "openai" {
		t.Fatalf("Chat() usage.Provider = %q, want openai", usage.Provider)
	}
}

func TestOpenAI_Chat_HTTP429(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limit exceeded"}}`)
	}

	o := NewOpenAI("test-key", openaiDefaultModel)
	o.chatClient = newMockClient(handler)

	_, _, err := o.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err == nil {
		t.Fatal("Chat() error = nil, want error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("Chat() error = %q, want it to mention 429", err.Error())
	}
	assertProviderErrorKind(t, err, ErrorKindRateLimit, http.StatusTooManyRequests)
}

func TestOpenAI_IsAvailable(t *testing.T) {
	if NewOpenAI("", "").IsAvailable() {
		t.Fatal("IsAvailable() = true for empty API key, want false")
	}
	if !NewOpenAI("key", "").IsAvailable() {
		t.Fatal("IsAvailable() = false for non-empty API key, want true")
	}
}

func assertProviderErrorKind(t *testing.T, err error, wantKind ErrorKind, wantStatus int) {
	t.Helper()

	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProviderError, got %T: %v", err, err)
	}
	if perr.Kind != wantKind {
		t.Fatalf("ProviderError.Kind = %q, want %q", perr.Kind, wantKind)
	}
	if perr.StatusCode != wantStatus {
		t.Fatalf("ProviderError.StatusCode = %d, want %d", perr.StatusCode, wantStatus)
	}
}
