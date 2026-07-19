package router

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controlledStreamProvider struct {
	name             string
	stream           <-chan StreamChunk
	startErr         error
	available        bool
	reportedModelID  string
	requestedModelID string
	started          chan struct{}
	startOnce        sync.Once
	streamCalls      atomic.Int32
}

type cancelAwareChatProvider struct {
	name          string
	started       chan struct{}
	startOnce     sync.Once
	waitForCancel bool
	chatCalls     atomic.Int32
}

func (p *cancelAwareChatProvider) Name() string      { return p.name }
func (p *cancelAwareChatProvider) IsAvailable() bool { return true }

func (p *cancelAwareChatProvider) Chat(ctx context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	p.chatCalls.Add(1)
	if p.started != nil {
		p.startOnce.Do(func() { close(p.started) })
	}
	if p.waitForCancel {
		<-ctx.Done()
		return "", Usage{}, ctx.Err()
	}
	return "fallback must not run", Usage{}, nil
}

func (p *cancelAwareChatProvider) ChatStream(context.Context, []Message, string, int) (<-chan StreamChunk, error) {
	return bufferedStream(StreamChunk{Done: true}), nil
}

func (p *controlledStreamProvider) Name() string { return p.name }

func (p *controlledStreamProvider) IsAvailable() bool { return p.available }

func (p *controlledStreamProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	return "", Usage{}, fmt.Errorf("%s non-stream chat is unsupported", p.name)
}

func (p *controlledStreamProvider) ChatStream(ctx context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	p.streamCalls.Add(1)
	if p.started != nil {
		p.startOnce.Do(func() { close(p.started) })
	}
	p.requestedModelID, _ = ModelFromContext(ctx)
	return p.stream, p.startErr
}

func (p *controlledStreamProvider) ReportedModelID(requestedModelID string) string {
	if p.reportedModelID != "" {
		return p.reportedModelID
	}
	return requestedModelID
}

func bufferedStream(chunks ...StreamChunk) <-chan StreamChunk {
	ch := make(chan StreamChunk, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	return ch
}

func newControlledStreamRouter(quota QuotaController, providers ...*controlledStreamProvider) *Router {
	entries := make(map[string]ProviderEntry, len(providers))
	for _, provider := range providers {
		entries[provider.name] = ProviderEntry{Provider: provider, Access: AccessSubscription}
	}
	return mustNewRouter(RouterConfig{
		Providers:    entries,
		DefaultModel: "claude",
		CodingModel:  "claude",
		Quota:        quota,
	})
}

func TestChatStream_StreamErrorDoesNotRecordSuccess(t *testing.T) {
	quota := NewQuotaSnapshotter(time.Hour)
	rateErr := newProviderError("claude", "stream", ErrorKindRateLimit, true, 429, "quota exhausted", nil)
	primary := &controlledStreamProvider{
		name:      "claude",
		available: true,
		stream: bufferedStream(
			StreamChunk{Content: "partial"},
			StreamChunk{Error: rateErr, Done: true},
		),
	}
	r := newControlledStreamRouter(quota, primary)
	r.breaker = newProviderCircuitBreaker(2, time.Hour)
	if opened, _ := r.breaker.RecordFailure("claude"); opened {
		t.Fatal("first seeded failure unexpectedly opened circuit")
	}

	var (
		traceMu sync.Mutex
		traces  []TraceEvent
	)
	r.SetTraceSink(TraceSinkFunc(func(event TraceEvent) {
		traceMu.Lock()
		traces = append(traces, event)
		traceMu.Unlock()
	}))

	stream, result, err := r.ChatStream(
		context.Background(),
		TaskCode,
		[]Message{{Role: "user", Content: "test"}},
		"",
	)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if result.Actual != "claude" {
		t.Fatalf("ChatStream() provider = %q, want claude", result.Actual)
	}

	var gotError error
	for chunk := range stream {
		if chunk.Error != nil {
			gotError = chunk.Error
		}
	}
	if gotError == nil {
		t.Fatal("stream did not expose terminal provider error")
	}
	if got := r.usage.Count("claude"); got != 0 {
		t.Fatalf("successful usage count = %d, want 0", got)
	}
	if got := r.usage.FailureCount("claude"); got != 1 {
		t.Fatalf("failure count = %d, want 1", got)
	}
	if blocked, _ := r.isCircuitOpen("claude"); !blocked {
		t.Fatal("terminal stream error did not preserve prior failure and open circuit")
	}
	if sealed, _ := r.quotaHardBlocked("claude"); !sealed {
		t.Fatal("terminal rate-limit error did not feed back into quota seal")
	}

	traceMu.Lock()
	defer traceMu.Unlock()
	var successEvents, errorEvents int
	for _, event := range traces {
		switch event.Event {
		case "chat_stream_start_success":
			successEvents++
		case "chat_stream_start_error":
			errorEvents++
		}
	}
	if successEvents != 0 {
		t.Fatalf("success trace count = %d, want 0", successEvents)
	}
	if errorEvents != 1 {
		t.Fatalf("error trace count = %d, want 1", errorEvents)
	}
}

func TestChatStream_FallsBackOnErrorBeforeFirstToken(t *testing.T) {
	primaryErr := newProviderError("claude", "stream", ErrorKindNetwork, true, 0, "connection reset", nil)
	primary := &controlledStreamProvider{
		name:      "claude",
		available: true,
		stream:    bufferedStream(StreamChunk{Error: primaryErr, Done: true}),
	}
	fallback := &controlledStreamProvider{
		name:      "gemini",
		available: true,
		stream:    bufferedStream(StreamChunk{Content: "fallback response", Done: true}),
	}
	r := newControlledStreamRouter(nil, primary, fallback)

	stream, result, err := r.ChatStream(
		context.Background(),
		TaskCode,
		[]Message{{Role: "user", Content: "test"}},
		"",
	)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if result.Actual != "gemini" || !result.IsFallback {
		t.Fatalf("ChatStream() route = %+v, want gemini fallback", result)
	}

	var content string
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatalf("fallback stream error = %v", chunk.Error)
		}
		content += chunk.Content
	}
	if content != "fallback response" {
		t.Fatalf("stream content = %q, want fallback response", content)
	}
	if got := r.usage.FailureCount("claude"); got != 1 {
		t.Fatalf("primary failure count = %d, want 1", got)
	}
	if got := r.usage.Count("claude"); got != 0 {
		t.Fatalf("primary success count = %d, want 0", got)
	}
	if got := r.usage.Count("gemini"); got != 1 {
		t.Fatalf("fallback success count = %d, want 1", got)
	}
}

func TestChatStream_RecordsSuccessOnlyAtTerminalDone(t *testing.T) {
	source := make(chan StreamChunk, 1)
	source <- StreamChunk{Content: "first token"}
	primary := &controlledStreamProvider{name: "claude", available: true, stream: source}
	r := newControlledStreamRouter(nil, primary)

	stream, _, err := r.ChatStream(
		context.Background(),
		TaskCode,
		[]Message{{Role: "user", Content: "test"}},
		"",
	)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	first := <-stream
	if first.Content != "first token" {
		t.Fatalf("first chunk = %+v, want first token", first)
	}
	if got := r.usage.Count("claude"); got != 0 {
		t.Fatalf("success count after first token = %d, want 0 before terminal done", got)
	}

	source <- StreamChunk{Done: true}
	close(source)
	for range stream {
	}
	if got := r.usage.Count("claude"); got != 1 {
		t.Fatalf("success count after terminal done = %d, want 1", got)
	}
	if got := r.usage.FailureCount("claude"); got != 0 {
		t.Fatalf("failure count = %d, want 0", got)
	}
}

func TestChatStream_CallerCancellationDoesNotPenalizeProvider(t *testing.T) {
	source := make(chan StreamChunk, 1)
	source <- StreamChunk{Content: "first token"}
	primary := &controlledStreamProvider{name: "claude", available: true, stream: source}
	r := newControlledStreamRouter(nil, primary)

	ctx, cancel := context.WithCancel(context.Background())
	stream, _, err := r.ChatStream(
		ctx,
		TaskCode,
		[]Message{{Role: "user", Content: "test"}},
		"",
	)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if first := <-stream; first.Content != "first token" {
		t.Fatalf("first chunk = %+v, want first token", first)
	}
	cancel()
	for range stream {
	}
	close(source)

	if got := r.usage.Count("claude"); got != 0 {
		t.Fatalf("success count after caller cancellation = %d, want 0", got)
	}
	if got := r.usage.FailureCount("claude"); got != 0 {
		t.Fatalf("failure count after caller cancellation = %d, want 0", got)
	}
}

func TestTryStreamProvider_InjectsRequestedModelButReportsActualModel(t *testing.T) {
	provider := &controlledStreamProvider{
		name:            "codex",
		available:       true,
		stream:          bufferedStream(StreamChunk{Done: true}),
		reportedModelID: "codex-provider-managed",
	}
	r := newControlledStreamRouter(nil, provider)

	var (
		traceMu sync.Mutex
		traces  []TraceEvent
	)
	r.SetTraceSink(TraceSinkFunc(func(event TraceEvent) {
		traceMu.Lock()
		traces = append(traces, event)
		traceMu.Unlock()
	}))

	res := r.tryStreamProvider(&attemptContext{
		ctx:       context.Background(),
		mode:      ModeBalanced,
		requested: "codex",
		labels: attemptLabels{
			attemptSuccess: "stream_success",
			attemptError:   "stream_error",
		},
	}, attemptIdentity{
		name:     "codex",
		modelID:  "gpt-requested-tier",
		provider: provider,
	})
	if res.err != nil {
		t.Fatalf("tryStreamProvider() error = %v", res.err)
	}
	for range res.ch {
	}

	if provider.requestedModelID != "gpt-requested-tier" {
		t.Fatalf("provider context model = %q, want requested model", provider.requestedModelID)
	}
	if res.route.ModelID != "codex-provider-managed" {
		t.Fatalf("route model = %q, want reported model", res.route.ModelID)
	}

	traceMu.Lock()
	defer traceMu.Unlock()
	if len(traces) != 1 {
		t.Fatalf("trace count = %d, want 1; traces=%+v", len(traces), traces)
	}
	if traces[0].ModelID != "codex-provider-managed" {
		t.Fatalf("trace model = %q, want reported model", traces[0].ModelID)
	}
}

func TestChatStream_CallerCancelBeforeFirstTokenDoesNotStartFallback(t *testing.T) {
	primary := &controlledStreamProvider{
		name:      "claude",
		available: true,
		stream:    make(chan StreamChunk),
		started:   make(chan struct{}),
	}
	fallback := &controlledStreamProvider{
		name:      "gemini",
		available: true,
		stream:    bufferedStream(StreamChunk{Content: "must not run", Done: true}),
	}
	r := newControlledStreamRouter(nil, primary, fallback)
	ctx, cancel := context.WithCancel(context.Background())
	type callResult struct {
		err error
	}
	resultCh := make(chan callResult, 1)
	go func() {
		_, _, err := r.ChatStream(ctx, TaskCode, []Message{{Role: "user", Content: "test"}}, "")
		resultCh <- callResult{err: err}
	}()

	<-primary.started
	cancel()
	result := <-resultCh
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("ChatStream error = %v, want context.Canceled", result.err)
	}
	if got := fallback.streamCalls.Load(); got != 0 {
		t.Fatalf("fallback stream calls = %d, want 0 after caller cancellation", got)
	}
	if got := r.usage.FailureCount("claude"); got != 0 {
		t.Fatalf("primary failure count = %d, want 0 for caller cancellation", got)
	}
}

func TestProviderAttemptTimeout_PowerRemoteCoversGeneratorAndJudge(t *testing.T) {
	local := providerAttemptTimeout(ModePower, PhaseReview, "claude")
	remote := providerAttemptTimeout(ModePower, PhaseReview, "remote")
	if remote < 2*local {
		t.Fatalf("Power remote timeout = %s, want at least two local phases (%s)", remote, 2*local)
	}
}

func TestChat_CallerCancelDoesNotStartFallbackOrPenalizeProvider(t *testing.T) {
	primary := &cancelAwareChatProvider{
		name:          "claude",
		started:       make(chan struct{}),
		waitForCancel: true,
	}
	fallback := &cancelAwareChatProvider{name: "gemini"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: primary, Access: AccessSubscription},
			"gemini": {Provider: fallback, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, _, _, err := r.Chat(ctx, TaskCode, []Message{{Role: "user", Content: "test"}}, "")
		resultCh <- err
	}()

	<-primary.started
	cancel()
	if err := <-resultCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat error = %v, want context.Canceled", err)
	}
	if got := fallback.chatCalls.Load(); got != 0 {
		t.Fatalf("fallback chat calls = %d, want 0 after caller cancellation", got)
	}
	if got := r.usage.FailureCount("claude"); got != 0 {
		t.Fatalf("primary failure count = %d, want 0 for caller cancellation", got)
	}
}
