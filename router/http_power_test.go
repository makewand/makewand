package router

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/makewand/makewand/serverauth"
)

func newHTTPPowerReviewFixture() (*Router, *ensembleCaptureProvider) {
	gemini := &ensembleCaptureProvider{
		name:      "gemini",
		available: true,
		content:   "gemini candidate",
		usage:     Usage{Provider: "gemini", Cost: 1, InputTokens: 1, OutputTokens: 2},
	}
	claude := &ensembleCaptureProvider{
		name:      "claude",
		available: true,
		content:   "claude winning candidate",
		usage:     Usage{Provider: "claude", Cost: 2, InputTokens: 3, OutputTokens: 4},
	}
	judge := &ensembleCaptureProvider{
		name:      "codex",
		available: true,
		content:   "WINNER: 2\nClaude follows the original request.",
		usage:     Usage{Provider: "codex", Cost: 3, InputTokens: 5, OutputTokens: 6},
	}
	return powerReviewRouter(gemini, claude, judge), judge
}

func TestHTTPPowerRunsEnsemble(t *testing.T) {
	r, judge := newHTTPPowerReviewFixture()
	handler := r.HTTPHandler()
	body := `{"mode":"power","messages":[{"role":"user","content":"review HTTP_POWER_ORIGINAL"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var response httpChatResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := response.Choices[0].Message.Content; got != "claude winning candidate" {
		t.Fatalf("content = %q, want judged winner", got)
	}
	if response.Usage.TotalTokens != 21 {
		t.Fatalf("total tokens = %d, want all ensemble calls (21)", response.Usage.TotalTokens)
	}
	judge.mu.Lock()
	judgePrompt := judge.messages[0].Content
	judge.mu.Unlock()
	if !strings.Contains(judgePrompt, "HTTP_POWER_ORIGINAL") {
		t.Fatal("HTTP Power judge did not receive the original request")
	}
}

func TestHTTPPowerLogsUsageWhenAllGeneratorsEmptyAndFallbackFails(t *testing.T) {
	r := newAllEmptyPowerErrorRouter()
	usageLog := &usageRecorder{}
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: usageLog})
	body := `{"mode":"power","messages":[{"role":"user","content":"review empty candidates"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if len(usageLog.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(usageLog.entries))
	}
	entry := usageLog.entries[0]
	if entry.ActualProvider != "ensemble" || entry.PromptTokens != 30 || entry.CompletionTokens != 3 || entry.CostUSD != 3.75 {
		t.Fatalf("failed Power usage entry = %+v, want complete ensemble usage", entry)
	}
}

func TestHTTPPowerStreamsLogUsageWhenAllGeneratorsEmptyAndFallbackFails(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: `{"mode":"power","stream":true,"messages":[{"role":"user","content":"review empty candidates"}]}`,
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: `{"mode":"power","stream":true,"input":"review empty candidates"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newAllEmptyPowerErrorRouter()
			usageLog := &usageRecorder{}
			handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: usageLog})
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
			}
			if len(usageLog.entries) != 1 {
				t.Fatalf("usage entries = %d, want 1", len(usageLog.entries))
			}
			entry := usageLog.entries[0]
			if entry.ActualProvider != "ensemble" || entry.PromptTokens != 30 || entry.CompletionTokens != 3 || entry.CostUSD != 3.75 {
				t.Fatalf("failed streamed Power usage entry = %+v, want complete ensemble usage", entry)
			}
		})
	}
}

func TestHTTPPowerStreamFailureChargesGrantBudget(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:            "power-stream-token",
				Scopes:           []string{serverauth.ScopeChatInvoke},
				MaxCostUSDPerDay: 3.75,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	r := newAllEmptyPowerErrorRouter()
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz})
	body := `{"mode":"power","stream":true,"messages":[{"role":"user","content":"review empty candidates"}]}`

	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	first.Header.Set("Authorization", "Bearer power-stream-token")
	first.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, want 503; body: %s", firstRec.Code, firstRec.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	second.Header.Set("Authorization", "Bearer power-stream-token")
	second.Header.Set("Content-Type", "application/json")
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body: %s", secondRec.Code, secondRec.Body.String())
	}
	if !strings.Contains(secondRec.Body.String(), "max_cost_usd_per_day") {
		t.Fatalf("second body = %q, want cost budget error", secondRec.Body.String())
	}
}

func TestHTTPPowerStreamReturnsJudgedWinner(t *testing.T) {
	r, _ := newHTTPPowerReviewFixture()
	handler := r.HTTPHandler()
	body := `{"mode":"power","stream":true,"messages":[{"role":"user","content":"review streamed result"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	streamBody := rec.Body.String()
	if !strings.Contains(streamBody, `"content":"claude winning candidate"`) {
		t.Fatalf("stream missing judged winner: %s", streamBody)
	}
	if strings.Contains(streamBody, `"content":"gemini candidate"`) {
		t.Fatalf("stream leaked a losing candidate: %s", streamBody)
	}
	if !strings.Contains(streamBody, "data: [DONE]") {
		t.Fatalf("stream missing terminal marker: %s", streamBody)
	}
}

func TestRemotePowerPreservesEnsembleSemantics(t *testing.T) {
	r, judge := newHTTPPowerReviewFixture()
	server := httptest.NewServer(r.HTTPHandler())
	defer server.Close()
	remote := NewRemoteHTTP(server.URL, "unused-test-token")
	ctx := ContextWithUsageMode(context.Background(), ModePower)

	content, usage, err := remote.Chat(ctx, []Message{{Role: "user", Content: "review REMOTE_POWER_ORIGINAL"}}, "", 4096)
	if err != nil {
		t.Fatalf("remote Chat: %v", err)
	}
	if content != "claude winning candidate" {
		t.Fatalf("remote content = %q, want judged winner", content)
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 12 {
		t.Fatalf("remote usage = %+v, want aggregate ensemble tokens", usage)
	}
	judge.mu.Lock()
	judgePrompt := judge.messages[0].Content
	judge.mu.Unlock()
	if !strings.Contains(judgePrompt, "REMOTE_POWER_ORIGINAL") {
		t.Fatal("remote Power judge did not receive the original request")
	}
}

func TestRemotePowerStreamReturnsJudgedWinner(t *testing.T) {
	r, _ := newHTTPPowerReviewFixture()
	server := httptest.NewServer(r.HTTPHandler())
	defer server.Close()
	remote := NewRemoteHTTP(server.URL, "unused-test-token")
	ctx := ContextWithUsageMode(context.Background(), ModePower)

	stream, err := remote.ChatStream(ctx, []Message{{Role: "user", Content: "review remote stream"}}, "", 4096)
	if err != nil {
		t.Fatalf("remote ChatStream: %v", err)
	}
	var content strings.Builder
	var sawDone bool
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatalf("remote stream chunk error: %v", chunk.Error)
		}
		content.WriteString(chunk.Content)
		sawDone = sawDone || chunk.Done
	}
	if content.String() != "claude winning candidate" {
		t.Fatalf("remote stream content = %q, want judged winner", content.String())
	}
	if !sawDone {
		t.Fatal("remote Power stream did not terminate with Done")
	}
}

func TestRemoteOnlyRouterChatBestDelegatesPowerToServer(t *testing.T) {
	serverRouter, _ := newHTTPPowerReviewFixture()
	server := httptest.NewServer(serverRouter.HTTPHandler())
	defer server.Close()
	clientRouter := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"remote": {Provider: NewRemoteHTTP(server.URL, "unused-test-token"), Access: AccessAPI},
		},
		UsageMode: "power",
	})

	content, usage, result, err := clientRouter.ChatBest(
		context.Background(),
		PhaseReview,
		[]Message{{Role: "user", Content: "review remote-only router"}},
		"",
	)
	if err != nil {
		t.Fatalf("remote-only ChatBest: %v", err)
	}
	if content != "claude winning candidate" {
		t.Fatalf("content = %q, want server's judged winner", content)
	}
	if result.Actual != "remote" || usage.Provider != "remote" {
		t.Fatalf("client attribution result/usage = %+v/%+v, want remote", result, usage)
	}
	if result.ModelID != testModelID("claude", TierPremium) {
		t.Fatalf("client result model = %q, want server winner model %q", result.ModelID, testModelID("claude", TierPremium))
	}
}

func TestHTTPStreamRecordsEstimatedAPIUsage(t *testing.T) {
	provider := &stubProvider{name: "claude", available: true, response: "streamed API response"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: provider, Access: AccessAPI},
		},
		UsageMode: "balanced",
	})
	usageLog := &usageRecorder{}
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: usageLog})
	body := `{"model":"claude","stream":true,"messages":[{"role":"user","content":"write a helper"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(usageLog.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(usageLog.entries))
	}
	entry := usageLog.entries[0]
	if entry.PromptTokens <= 0 || entry.CompletionTokens <= 0 {
		t.Fatalf("stream usage tokens = %d/%d, want positive estimates", entry.PromptTokens, entry.CompletionTokens)
	}
	if entry.CostUSD <= 0 {
		t.Fatalf("stream API cost = %f, want positive estimate", entry.CostUSD)
	}
}

func TestHTTPResponsesStreamMarksUsageEntryAsStream(t *testing.T) {
	provider := &stubProvider{name: "claude", available: true, response: "responses stream"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: provider, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})
	usageLog := &usageRecorder{}
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: usageLog})
	body := `{"model":"claude","stream":true,"input":"explain this"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(usageLog.entries) != 1 || !usageLog.entries[0].Stream {
		t.Fatalf("responses usage entries = %+v, want one stream entry", usageLog.entries)
	}
}
