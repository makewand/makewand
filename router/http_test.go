package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/servermetrics"
	"github.com/makewand/makewand/serverteam"
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

func (r *usageRecorder) Log(entry serverusage.Entry) error {
	r.entries = append(r.entries, entry)
	return nil
}

// failingUsageLogger always fails to persist, standing in for a full disk or a
// locked/closed database.
type failingUsageLogger struct{ calls int }

func (f *failingUsageLogger) Log(serverusage.Entry) error {
	f.calls++
	return errors.New("disk full")
}

// TestHTTPHandler_UsageLogFailureSurfaces asserts that a failed usage write is
// reported through UsageLogErrorHandler rather than silently dropped, and that
// the request itself still succeeds (accounting runs after the response).
func TestHTTPHandler_UsageLogFailureSurfaces(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "ok"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	failing := &failingUsageLogger{}
	var gotErr error
	handler := r.HTTPHandler(HTTPHandlerOptions{
		UsageLogger:          failing,
		UsageLogErrorHandler: func(err error) { gotErr = err },
	})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if failing.calls != 1 {
		t.Fatalf("usage logger called %d times, want 1", failing.calls)
	}
	if gotErr == nil {
		t.Fatal("UsageLogErrorHandler was not invoked on a failed usage write")
	}
}

// countingUsageLogger records how many entries it stored and can be told to fail.
type countingUsageLogger struct {
	mu     sync.Mutex
	calls  int
	fail   bool
	stored []serverusage.Entry
}

func (c *countingUsageLogger) Log(e serverusage.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.fail {
		return errors.New("disk full")
	}
	c.stored = append(c.stored, e)
	return nil
}

// TestLogHTTPUsageSanitizesCost guards that a NaN/Inf/negative cost is never
// persisted to the ledger, where it would later poison a budget seed.
func TestLogHTTPUsageSanitizesCost(t *testing.T) {
	logger := &countingUsageLogger{}
	if err := logHTTPUsage(logger, serverusage.Entry{CostUSD: math.Inf(1)}); err != nil {
		t.Fatalf("logHTTPUsage: %v", err)
	}
	if len(logger.stored) != 1 {
		t.Fatalf("stored = %d, want 1", len(logger.stored))
	}
	if logger.stored[0].CostUSD != 0 {
		t.Fatalf("persisted cost = %v, want 0 (sanitized)", logger.stored[0].CostUSD)
	}
}

// TestStrictAccountingRejectsWhenUsageWriteFails: in strict mode a non-streaming
// request whose usage cannot be recorded is rejected with 503 and NO completion
// body — so no successful response is returned unrecorded.
func TestStrictAccountingRejectsWhenUsageWriteFails(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello", cost: 0.5}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	logger := &countingUsageLogger{fail: true}
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: logger, StrictAccounting: true})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("strict rejection must not include the model response; body: %s", rec.Body.String())
	}
	if logger.calls != 1 {
		t.Fatalf("usage logger calls = %d, want exactly 1 (attempted before response)", logger.calls)
	}
}

// sleepyProvider takes a fixed, non-trivial amount of time so the recorded
// request duration is deterministically positive.
type sleepyProvider struct {
	name string
	d    time.Duration
}

func (p *sleepyProvider) Name() string      { return p.name }
func (p *sleepyProvider) IsAvailable() bool { return true }

func (p *sleepyProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	time.Sleep(p.d)
	return "hi", Usage{Provider: p.name, InputTokens: 5, OutputTokens: 3, Cost: 0.5}, nil
}

func (p *sleepyProvider) ChatStream(context.Context, []Message, string, int) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: "hi", Done: true}
	close(ch)
	return ch, nil
}

// TestStrictAccountingRecordsPopulatedEntry proves the entry recorded BEFORE the
// response under strict mode carries the realized fields — including a populated
// (non-hardcoded) DurationMS — not a blank/zeroed entry.
func TestStrictAccountingRecordsPopulatedEntry(t *testing.T) {
	prov := &sleepyProvider{name: "claude", d: 3 * time.Millisecond}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: prov, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	logger := &countingUsageLogger{}
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: logger, StrictAccounting: true})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(logger.stored) != 1 {
		t.Fatalf("stored entries = %d, want 1", len(logger.stored))
	}
	e := logger.stored[0]
	if e.CostUSD != 0.5 || e.PromptTokens != 5 || e.CompletionTokens != 3 {
		t.Fatalf("recorded entry not fully populated: cost=%.2f in=%d out=%d", e.CostUSD, e.PromptTokens, e.CompletionTokens)
	}
	if e.DurationMS <= 0 {
		t.Fatalf("recorded DurationMS = %d, want > 0 (populated before response, not hardcoded 0)", e.DurationMS)
	}
}

// observerOnlyLogger returns nil from Log without persisting and declares itself
// non-durable — like an alert notifier.
type observerOnlyLogger struct{}

func (observerOnlyLogger) Log(serverusage.Entry) error { return nil }
func (observerOnlyLogger) Durable() bool               { return false }

// TestStrictAccountingRejectsObserverOnlySink: strict mode must reject when the
// only usage sink is a non-persisting observer (which would return nil without
// recording anything).
func TestStrictAccountingRejectsObserverOnlySink(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: observerOnlyLogger{}, StrictAccounting: true})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (observer-only sink under strict); body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("strict rejection must not include the model response; body: %s", rec.Body.String())
	}
}

// TestStrictAccountingRejectsWhenNoUsageSink: strict mode with no UsageLogger
// cannot record anything, so it must reject rather than falsely "succeed".
func TestStrictAccountingRejectsWhenNoUsageSink(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{StrictAccounting: true}) // no UsageLogger

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no usage sink under strict); body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("strict rejection must not include the model response; body: %s", rec.Body.String())
	}
}

// TestStrictAccountingResponsesRejectsWhenUsageWriteFails covers the /v1/responses
// endpoint (not just /v1/chat/completions) under strict accounting.
func TestStrictAccountingResponsesRejectsWhenUsageWriteFails(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello", cost: 0.5}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	logger := &countingUsageLogger{fail: true}
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: logger, StrictAccounting: true})

	body := `{"model":"claude","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("strict rejection must not include the model response; body: %s", rec.Body.String())
	}
	if logger.calls != 1 {
		t.Fatalf("usage logger calls = %d, want 1", logger.calls)
	}
}

// TestStrictAccountingLogsExactlyOnceOnSuccess: strict mode logs before the
// write and must not double-log in the deferred finalizer.
func TestStrictAccountingLogsExactlyOnceOnSuccess(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	logger := &countingUsageLogger{}
	handler := r.HTTPHandler(HTTPHandlerOptions{UsageLogger: logger, StrictAccounting: true})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if logger.calls != 1 {
		t.Fatalf("usage logged %d times, want exactly 1", logger.calls)
	}
}

// TestNonStrictAccountingStillSucceedsOnUsageFailure: default (non-strict) mode
// keeps the request successful even when usage logging fails; the failure is
// surfaced via the error handler, not the response.
func TestNonStrictAccountingStillSucceedsOnUsageFailure(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	logger := &countingUsageLogger{fail: true}
	var gotErr error
	handler := r.HTTPHandler(HTTPHandlerOptions{
		UsageLogger:          logger,
		UsageLogErrorHandler: func(err error) { gotErr = err },
	})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (non-strict); body: %s", rec.Code, rec.Body.String())
	}
	if gotErr == nil {
		t.Fatal("usage failure should still be surfaced via UsageLogErrorHandler")
	}
}

func TestHTTPHandler_ChatCompletions(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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

func TestHTTPHandler_ChatCompletionsStream_WithMetricsMiddleware(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := servermetrics.NewRecorder().Middleware(r.HTTPHandler())
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

func TestHTTPHandler_ResponsesSubset(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello from responses"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{"model":"claude","instructions":"be concise","input":"hi","metadata":{"trace":"1"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp httpResponsesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Object != "response" {
		t.Fatalf("Object = %q, want response", resp.Object)
	}
	if resp.OutputText != "hello from responses" {
		t.Fatalf("OutputText = %q, want %q", resp.OutputText, "hello from responses")
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "hello from responses" {
		t.Fatalf("Output = %+v, want assistant output text", resp.Output)
	}
	if resp.Metadata["trace"] != "1" {
		t.Fatalf("Metadata = %+v, want trace=1", resp.Metadata)
	}
}

func TestHTTPHandler_ResponsesStream(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "hello from responses stream"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{"model":"claude","stream":true,"input":"hi","metadata":{"trace":"1"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
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
	if !strings.Contains(bodyText, "event: response.created") {
		t.Fatalf("stream body missing response.created event: %s", bodyText)
	}
	if !strings.Contains(bodyText, "event: response.output_text.delta") {
		t.Fatalf("stream body missing response.output_text.delta event: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"delta":"hello from responses stream"`) {
		t.Fatalf("stream body missing response text: %s", bodyText)
	}
	if !strings.Contains(bodyText, "event: response.completed") {
		t.Fatalf("stream body missing response.completed event: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"output_text":"hello from responses stream"`) {
		t.Fatalf("stream body missing completed output text: %s", bodyText)
	}
}

func TestHTTPHandler_ModelAliasResolvesToProvider(t *testing.T) {
	codex := &stubProvider{name: "codex", available: true, response: "hello from codex"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"codex": {Provider: codex, Access: AccessSubscription},
		},
		DefaultModel: "codex",
		CodingModel:  "codex",
	})

	handler := r.HTTPHandler()
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
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
	if resp.Choices[0].Message.Content != "hello from codex" {
		t.Fatalf("content = %q, want %q", resp.Choices[0].Message.Content, "hello from codex")
	}
	if !strings.Contains(resp.Model, "codex") {
		t.Fatalf("model = %q, want codex-family model id", resp.Model)
	}
}

func TestHTTPHandler_ResponsesJSONSchema(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: `{"answer":"ok","score":1}`}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{
		"model":"claude",
		"input":"hi",
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"answer_payload",
				"schema":{
					"type":"object",
					"properties":{
						"answer":{"type":"string"},
						"score":{"type":"integer"}
					},
					"required":["answer","score"],
					"additionalProperties":false
				}
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp httpResponsesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OutputText != `{"answer":"ok","score":1}` {
		t.Fatalf("OutputText = %q, want normalized schema JSON", resp.OutputText)
	}
}

func TestHTTPHandler_ResponsesJSONSchemaRejectsInvalidOutput(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: `{"answer":"ok","extra":true}`}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{
		"model":"claude",
		"input":"hi",
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"answer_payload",
				"schema":{
					"type":"object",
					"properties":{"answer":{"type":"string"}},
					"required":["answer"],
					"additionalProperties":false
				}
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not allowed") {
		t.Fatalf("body = %q, want schema validation error", rec.Body.String())
	}
}

func TestHTTPHandler_ChatToolCalls(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: `{"tool_calls":[{"name":"lookup_weather","arguments":{"city":"Paris"}}]}`}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{
		"model":"claude",
		"messages":[{"role":"user","content":"weather?"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"lookup_weather",
					"description":"Look up weather",
					"parameters":{
						"type":"object",
						"properties":{"city":{"type":"string"}},
						"required":["city"]
					}
				}
			}
		]
	}`
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
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one tool call", resp.Choices[0].Message.ToolCalls)
	}
	call := resp.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "lookup_weather" {
		t.Fatalf("tool name = %q, want lookup_weather", call.Function.Name)
	}
	if !strings.Contains(call.Function.Arguments, `"city":"Paris"`) {
		t.Fatalf("arguments = %q, want city Paris", call.Function.Arguments)
	}
}

func TestHTTPHandler_ResponsesToolCalls(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: `{"tool_calls":[{"name":"lookup_weather","arguments":{"city":"Paris"}}]}`}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler()
	body := `{
		"model":"claude",
		"input":"weather?",
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"lookup_weather",
					"description":"Look up weather",
					"parameters":{
						"type":"object",
						"properties":{"city":{"type":"string"}}
					}
				}
			}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp httpResponsesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OutputText != "" {
		t.Fatalf("OutputText = %q, want empty for tool call output", resp.OutputText)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("Output = %+v, want one function_call item", resp.Output)
	}
	if resp.Output[0].Type != "function_call" || resp.Output[0].Name != "lookup_weather" {
		t.Fatalf("Output[0] = %+v, want function_call lookup_weather", resp.Output[0])
	}
	if !strings.Contains(resp.Output[0].Arguments, `"city":"Paris"`) {
		t.Fatalf("Arguments = %q, want city Paris", resp.Output[0].Arguments)
	}
}

func TestHTTPHandler_ModelOverrideUsesRequestedProvider(t *testing.T) {
	claude := &stubProvider{name: "claude", available: true, response: "hello from claude"}
	gemini := &stubProvider{name: "gemini", available: true, response: "hello from gemini"}
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{})

	handler := r.HTTPHandler()
	body := `{"model":"test","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHTTPHandler_RejectsOversizedChatBody(t *testing.T) {
	r := mustNewRouter(RouterConfig{})

	handler := r.HTTPHandler()
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", maxHTTPJSONBodyBytes) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Fatalf("body = %q, want request_too_large", rec.Body.String())
	}
}

func TestHTTPHandler_UnknownModelRejected(t *testing.T) {
	r := mustNewRouter(RouterConfig{})

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
	r := mustNewRouter(RouterConfig{})

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
	r := mustNewRouter(RouterConfig{})

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
	r := mustNewRouter(RouterConfig{})

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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{})

	handler := r.HTTPHandler()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHTTPHandler_BearerAuth_Rejects(t *testing.T) {
	r := mustNewRouter(RouterConfig{})

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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{})

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

	r := mustNewRouter(RouterConfig{})
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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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
	r := mustNewRouter(RouterConfig{
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

func TestHTTPHandler_LogsUsageOwnershipAttribution(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:             "runner",
				Token:          "secret123",
				UserID:         "usr_123",
				OrganizationID: "org_platform",
				ProjectID:      "prj_checkout",
				Scopes:         []string{serverauth.ScopeChatInvoke},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	recorder := &usageRecorder{}
	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, UsageLogger: recorder})
	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(recorder.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(recorder.entries))
	}
	entry := recorder.entries[0]
	if entry.UserID != "usr_123" || entry.OrganizationID != "org_platform" || entry.ProjectID != "prj_checkout" {
		t.Fatalf("ownership attribution = %+v, want user/org/project IDs", entry)
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
	r := mustNewRouter(RouterConfig{})
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

	r := mustNewRouter(RouterConfig{})
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

	r := mustNewRouter(RouterConfig{})
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

	r := mustNewRouter(RouterConfig{
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

func TestHTTPHandler_RejectsRequestsWhenProjectBudgetExceeded(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	teamStore, err := serverteam.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()
	usageStore, err := serverusage.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(usage): %v", err)
	}
	defer usageStore.Close()

	org, err := teamStore.CreateOrganization(serverteam.Organization{
		ID:               "org_platform",
		Name:             "Platform Team",
		MonthlyBudgetUSD: 100,
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	project, err := teamStore.CreateProject(serverteam.Project{
		ID:               "prj_checkout",
		OrganizationID:   org.ID,
		Name:             "Checkout API",
		MonthlyBudgetUSD: 1,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	_ = usageStore.Log(serverusage.Entry{
		Timestamp:      time.Now().UTC(),
		OrganizationID: org.ID,
		ProjectID:      project.ID,
		CostUSD:        1,
	})

	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:          "secret123",
				Scopes:         []string{serverauth.ScopeChatInvoke},
				OrganizationID: org.ID,
				ProjectID:      project.ID,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{
		Authorizer:  authz,
		UsageReader: usageStore,
		TeamStore:   teamStore,
	})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project") {
		t.Fatalf("body = %q, want project budget error", rec.Body.String())
	}
}

func TestHTTPHandler_ProjectOnlyTokenHonorsParentOrgBudget(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	teamStore, err := serverteam.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()
	usageStore, err := serverusage.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(usage): %v", err)
	}
	defer usageStore.Close()

	org, err := teamStore.CreateOrganization(serverteam.Organization{
		ID: "org_platform", Name: "Platform Team", MonthlyBudgetUSD: 1,
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	// Project itself has no cap (0) — only the parent organization is budgeted.
	project, err := teamStore.CreateProject(serverteam.Project{
		ID: "prj_checkout", OrganizationID: org.ID, Name: "Checkout API", MonthlyBudgetUSD: 0,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Organization is already at its cap (spend attributed to the org, e.g. from
	// other tokens/projects).
	_ = usageStore.Log(serverusage.Entry{Timestamp: time.Now().UTC(), OrganizationID: org.ID, CostUSD: 1})

	// Token carries ONLY the project scope — no explicit organization id.
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{{
			Token:     "secret123",
			Scopes:    []string{serverauth.ScopeChatInvoke},
			ProjectID: project.ID,
		}},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	stub := &stubProvider{name: "claude", available: true, response: "hi"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, UsageReader: usageStore, TeamStore: teamStore})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (parent org over budget); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "organization") {
		t.Fatalf("body = %q, want parent organization budget error", rec.Body.String())
	}
}

func TestHTTPHandler_BudgetFailsClosedOnLookupError(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	teamStore, err := serverteam.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()
	usageStore, err := serverusage.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(usage): %v", err)
	}
	defer usageStore.Close()

	// Token names a project that does not exist — GetProject errors. This must
	// fail closed (503), not be read as "no budget configured, allow".
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{{
			Token:     "secret123",
			Scopes:    []string{serverauth.ScopeChatInvoke},
			ProjectID: "prj_does_not_exist",
		}},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	stub := &stubProvider{name: "claude", available: true, response: "hi"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, UsageReader: usageStore, TeamStore: teamStore})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (budget unavailable, fail closed); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "budget_unavailable") {
		t.Fatalf("body = %q, want budget_unavailable", rec.Body.String())
	}
}

func TestHTTPHandler_TokenOrgProjectMismatchFailsClosed(t *testing.T) {
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

	orgA, _ := teamStore.CreateOrganization(serverteam.Organization{ID: "org_a", Name: "A"})
	orgB, _ := teamStore.CreateOrganization(serverteam.Organization{ID: "org_b", Name: "B"})
	// Project belongs to org B, but the token claims org A → misconfiguration.
	project, err := teamStore.CreateProject(serverteam.Project{ID: "prj_x", OrganizationID: orgB.ID, Name: "X"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{{
			Token: "sec", Scopes: []string{serverauth.ScopeChatInvoke},
			OrganizationID: orgA.ID, ProjectID: project.ID,
		}},
	})
	if err != nil {
		t.Fatalf("authorizer: %v", err)
	}

	stub := &stubProvider{name: "claude", available: true, response: "hi"}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, UsageReader: usageStore, TeamStore: teamStore})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer sec")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (org/project mismatch); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "scope_conflict") {
		t.Fatalf("body = %q, want scope_conflict", rec.Body.String())
	}
}

func TestHTTPHandler_ProjectBudgetIgnoresPreviousMonths(t *testing.T) {
	stateDB := filepath.Join(t.TempDir(), "state.db")
	teamStore, err := serverteam.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(team): %v", err)
	}
	defer teamStore.Close()
	usageStore, err := serverusage.OpenSQLiteStore(stateDB)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(usage): %v", err)
	}
	defer usageStore.Close()

	org, err := teamStore.CreateOrganization(serverteam.Organization{
		ID:               "org_platform",
		Name:             "Platform Team",
		MonthlyBudgetUSD: 100,
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	project, err := teamStore.CreateProject(serverteam.Project{
		ID:               "prj_checkout",
		OrganizationID:   org.ID,
		Name:             "Checkout API",
		MonthlyBudgetUSD: 1,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	now := time.Now().UTC()
	_ = usageStore.Log(serverusage.Entry{
		Timestamp:      serverusage.MonthStart(now).AddDate(0, -1, 0).Add(2 * time.Hour),
		OrganizationID: org.ID,
		ProjectID:      project.ID,
		CostUSD:        5,
	})

	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{{
			Token:          "secret123",
			Scopes:         []string{serverauth.ScopeChatInvoke},
			OrganizationID: org.ID,
			ProjectID:      project.ID,
		}},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	stub := &stubProvider{name: "claude", available: true, response: "hello from http"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	handler := r.HTTPHandler(HTTPHandlerOptions{
		Authorizer:  authz,
		UsageReader: usageStore,
		TeamStore:   teamStore,
	})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandler_MethodNotAllowed(t *testing.T) {
	r := mustNewRouter(RouterConfig{})

	handler := r.HTTPHandler()
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
