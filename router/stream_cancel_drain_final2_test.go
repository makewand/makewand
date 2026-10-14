package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Final round 2 (stream-cancel-writeback): a stream that a cancellation or a
// deadline ends, ends - its terminal chunk and the close of its channel -
// only once its source has, for a provider whose stream still does work after
// the cancellation that the caller must not outrun (a provider CLI: its
// sandbox's credential write-back). The wait is bounded, the caller still
// sees the cancellation's error, and other providers' streams end at once.

// slowEndingStreamProvider is a provider whose stream, once its context has
// ended, takes endAfter to end, as a provider CLI being killed and then
// writing back its sandbox's credentials does; ended records that it did,
// right before its channel closes. Its stream never ends on its own once
// release is closed.
type slowEndingStreamProvider struct {
	name      string
	first     bool // the stream sends a content chunk first
	endAfter  time.Duration
	release   chan struct{}
	ended     atomic.Bool
	started   chan struct{}
	startOnce sync.Once
}

func newSlowEndingStreamProvider(t *testing.T, name string, first bool, endAfter time.Duration) *slowEndingStreamProvider {
	p := &slowEndingStreamProvider{name: name, first: first, endAfter: endAfter, release: make(chan struct{}), started: make(chan struct{})}
	t.Cleanup(func() { close(p.release) })
	return p
}

func (p *slowEndingStreamProvider) Name() string      { return p.name }
func (p *slowEndingStreamProvider) IsAvailable() bool { return true }

func (p *slowEndingStreamProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	return "", Usage{}, errors.New("stream only")
}

func (p *slowEndingStreamProvider) ChatStream(ctx context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	go func() {
		defer close(ch)
		if p.first {
			ch <- StreamChunk{Content: "first"}
		}
		p.startOnce.Do(func() { close(p.started) })
		<-ctx.Done()
		timer := time.NewTimer(p.endAfter)
		defer timer.Stop()
		select {
		case <-timer.C:
			p.ended.Store(true)
		case <-p.release:
		}
	}()
	return ch, nil
}

// slowEndingWaiterProvider is a slowEndingStreamProvider whose streams, like
// a provider CLI's, are waited for after a cancellation for at most wait.
type slowEndingWaiterProvider struct {
	*slowEndingStreamProvider
	wait time.Duration
}

func (p *slowEndingWaiterProvider) streamEndWait() time.Duration { return p.wait }

func newSlowEndingWaiterProvider(t *testing.T, first bool, endAfter, wait time.Duration) *slowEndingWaiterProvider {
	return &slowEndingWaiterProvider{slowEndingStreamProvider: newSlowEndingStreamProvider(t, "claude", first, endAfter), wait: wait}
}

func slowEndingRouter(p Provider) *Router {
	return mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: p, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
}

// readStreamToClose reads stream until it closes and returns its last chunk.
func readStreamToClose(t *testing.T, stream <-chan StreamChunk, limit time.Duration) StreamChunk {
	t.Helper()
	var last StreamChunk
	timeout := time.After(limit)
	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return last
			}
			last = chunk
		case <-timeout:
			t.Fatalf("stream did not close within %v", limit)
		}
	}
}

// A Router stream cancelled after its first chunk closes only once the
// provider's stream has ended, with the cancellation as its error and no
// penalty for the provider.
func TestCancelledRouterStreamEndsAfterItsSource_Final2(t *testing.T) {
	p := newSlowEndingWaiterProvider(t, true, 150*time.Millisecond, 10*time.Second)
	r := slowEndingRouter(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, _, err := r.ChatStream(ctx, TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if first := <-stream; first.Content != "first" {
		t.Fatalf("first chunk = %+v", first)
	}
	cancel()
	last := readStreamToClose(t, stream, 5*time.Second)
	if !p.ended.Load() {
		t.Fatal("the stream closed before the provider's stream had ended")
	}
	if last.Error != nil && !errors.Is(last.Error, context.Canceled) {
		t.Fatalf("terminal chunk error = %v, want the cancellation", last.Error)
	}
	if got := r.usage.FailureCount("claude"); got != 0 {
		t.Fatalf("failure count after a caller cancellation = %d, want 0", got)
	}
}

// Cancelled before the first chunk, Router.ChatStream returns its error only
// once the provider's stream has ended, and starts no fallback.
func TestCancelledRouterStreamStartEndsAfterItsSource_Final2(t *testing.T) {
	p := newSlowEndingWaiterProvider(t, false, 150*time.Millisecond, 10*time.Second)
	r := slowEndingRouter(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := r.ChatStream(ctx, TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
		done <- err
	}()
	<-p.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ChatStream error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatStream did not return after the cancellation")
	}
	if !p.ended.Load() {
		t.Fatal("ChatStream returned before the provider's stream had ended")
	}
}

// A deadline ends a provider stream (ChatStreamProvider, the dispatch
// wrapper) once the source has ended, with the deadline as its error.
func TestDeadlinedProviderStreamEndsAfterItsSource_Final2(t *testing.T) {
	p := newSlowEndingWaiterProvider(t, true, 150*time.Millisecond, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stream, err := ChatStreamProvider(ctx, p, []Message{{Role: "user", Content: "hi"}}, "", 64)
	if err != nil {
		t.Fatalf("ChatStreamProvider: %v", err)
	}
	last := readStreamToClose(t, stream, 5*time.Second)
	if !p.ended.Load() {
		t.Fatal("the stream closed before the provider's stream had ended")
	}
	if !last.Done || !errors.Is(last.Error, context.DeadlineExceeded) {
		t.Fatalf("terminal chunk = %+v, want the deadline", last)
	}
}

// A source that does not end cannot hang the cancellation: the stream ends
// after the provider's bound.
func TestCancelledStreamWaitIsBounded_Final2(t *testing.T) {
	const wait = 300 * time.Millisecond
	p := newSlowEndingWaiterProvider(t, true, time.Hour, wait)
	r := slowEndingRouter(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, _, err := r.ChatStream(ctx, TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	<-stream
	start := time.Now()
	cancel()
	readStreamToClose(t, stream, 5*time.Second)
	if took := time.Since(start); took < wait*3/4 || took > wait+3*time.Second {
		t.Fatalf("the cancelled stream closed after %v, want about the bound %v", took, wait)
	}
	if p.ended.Load() {
		t.Fatal("the source ended though it never does")
	}
}

// A provider without a stream end wait (an API provider) is not waited for:
// a cancellation ends its stream at once.
func TestCancelledAPIStreamEndsAtOnce_Final2(t *testing.T) {
	p := newSlowEndingStreamProvider(t, "claude", true, time.Hour)
	if _, ok := Provider(p).(streamEndWaiter); ok {
		t.Fatal("fixture must not wait for its streams")
	}
	r := slowEndingRouter(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, _, err := r.ChatStream(ctx, TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	<-stream
	start := time.Now()
	cancel()
	readStreamToClose(t, stream, 5*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the cancelled API stream closed after %v, want at once", took)
	}
}

// Provider CLIs are the providers whose streams are waited for, for the limit
// of their credential write-back.
func TestCLIProviderStreamEndWaitIsTheWriteBackLimit_Final2(t *testing.T) {
	if got, want := streamDrainWaitFor(NewCommandCLI("custom", "/bin/true", nil, PromptModeArg)), cliWriteBackWait(); got != want || got <= 0 {
		t.Fatalf("CLI stream end wait = %v, want the write-back limit %v", got, want)
	}
	for _, p := range []Provider{NewClaude("", ""), NewOpenAI("", ""), NewGemini("", ""), NewRemoteHTTP("http://127.0.0.1:1", "")} {
		if got := streamDrainWaitFor(p); got != 0 {
			t.Fatalf("%T stream end wait = %v, want none", p, got)
		}
	}
}
