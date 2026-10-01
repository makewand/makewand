package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type concurrencyProvider struct {
	name                string
	active, peak, calls atomic.Int64
	entered             chan struct{}
	release             chan struct{}
	delay               time.Duration
}

func (p *concurrencyProvider) Name() string      { return p.name }
func (p *concurrencyProvider) IsAvailable() bool { return true }
func (p *concurrencyProvider) begin() {
	active := p.active.Add(1)
	p.calls.Add(1)
	for {
		old := p.peak.Load()
		if active <= old || p.peak.CompareAndSwap(old, active) {
			break
		}
	}
	if p.entered != nil {
		p.entered <- struct{}{}
	}
}
func (p *concurrencyProvider) Chat(ctx context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	p.begin()
	defer p.active.Add(-1)
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	} else {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	}
	return "ok", Usage{Provider: p.name}, nil
}
func (p *concurrencyProvider) ChatStream(ctx context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	p.begin()
	out := make(chan StreamChunk, 1)
	go func() {
		defer close(out)
		defer p.active.Add(-1)
		select {
		case out <- StreamChunk{Content: "first"}:
		case <-ctx.Done():
			return
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		select {
		case out <- StreamChunk{Done: true}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

func TestBulkheadDefaultsAndRequestClonesShareLimits(t *testing.T) {
	for _, tc := range []struct {
		access AccessType
		limit  int64
	}{{AccessAPI, 8}, {AccessSubscription, 1}} {
		p := &concurrencyProvider{name: "claude", delay: time.Millisecond}
		r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: p, Access: tc.access}}, DefaultModel: "claude"})
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			clone := r.cloneView()
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, _, err := clone.ChatWith(context.Background(), "claude", PhaseCode, nil, ""); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if p.peak.Load() > tc.limit || p.calls.Load() != 64 {
			t.Fatalf("access=%d peak=%d limit=%d calls=%d", tc.access, p.peak.Load(), tc.limit, p.calls.Load())
		}
		if err := r.ConfigureConcurrency(64, 64, time.Second); err == nil {
			t.Fatal("active router replaced semaphore")
		}
	}
}

func TestBulkheadQueueCancellationDoesNotDispatchOrSpendCallBudget(t *testing.T) {
	ctx, path := budgetTestContext(t, 4)
	p := &concurrencyProvider{name: "claude", entered: make(chan struct{}, 4), release: make(chan struct{})}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: p, Access: AccessAPI}}, DefaultModel: "claude", APIConcurrency: 1})
	done := make(chan error, 1)
	go func() { _, _, _, err := r.ChatWith(ctx, "claude", PhaseCode, nil, ""); done <- err }()
	<-p.entered
	cancelCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, _, _, err := r.cloneView().ChatWith(cancelCtx, "claude", PhaseCode, nil, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queue cancel: %v", err)
	}
	if p.calls.Load() != 1 || len(budgetTestAttempts(t, path)) != 1 {
		t.Fatal("queued cancellation dispatched or consumed a call")
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.ChatWith(ctx, "claude", PhaseCode, nil, ""); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 2 {
		t.Fatal("permit leaked")
	}
}

func TestBulkheadQueueTimeoutFallsBackWithoutPoisoningHealth(t *testing.T) {
	p := &concurrencyProvider{name: "claude", entered: make(chan struct{}, 2), release: make(chan struct{})}
	fallback := &stubProvider{name: "gemini", available: true, response: "fallback"}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: p, Access: AccessAPI}, "gemini": {Provider: fallback, Access: AccessAPI}}, DefaultModel: "claude", APIConcurrency: 1, ProviderQueueTimeout: 10 * time.Millisecond})
	done := make(chan error, 1)
	go func() { _, _, _, err := r.ChatWith(context.Background(), "claude", PhaseCode, nil, ""); done <- err }()
	<-p.entered
	content, _, route, err := r.ChatWith(context.Background(), "claude", PhaseCode, nil, "")
	if err != nil || content != "fallback" || route.Actual != "gemini" {
		t.Fatalf("queue timeout did not fall back: %q %+v %v", content, route, err)
	}
	if open, _ := r.isCircuitOpen("claude"); open {
		t.Fatal("queue timeout poisoned provider health")
	}
	close(p.release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBulkheadStreamHoldsPermitUntilCancellation(t *testing.T) {
	p := &concurrencyProvider{name: "claude", entered: make(chan struct{}, 4), release: make(chan struct{})}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: p, Access: AccessAPI}}, DefaultModel: "claude", APIConcurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	stream, _, err := r.ChatStreamWith(ctx, "claude", PhaseCode, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	<-p.entered
	<-stream
	queued, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if _, _, _, err = r.ChatWith(queued, "claude", PhaseCode, nil, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stream permit released after first chunk: %v", err)
	}
	if p.calls.Load() != 1 {
		t.Fatal("another call bypassed active stream")
	}
	cancel()
	for range stream {
	}
	close(p.release)
	if _, _, _, err = r.ChatWith(context.Background(), "claude", PhaseCode, nil, ""); err != nil {
		t.Fatalf("stream cancellation leaked permit: %v", err)
	}
}

func TestBulkheadHalfOpenCancellationReleasesNeutralLease(t *testing.T) {
	cb := newProviderCircuitBreaker(1, time.Millisecond)
	clock := time.Now()
	cb.now = func() time.Time { return clock }
	cb.RecordFailure("claude")
	clock = clock.Add(time.Second)
	allow, _, ticket := cb.Admit("claude")
	if !allow || !ticket.probe {
		t.Fatal("probe not admitted")
	}
	cb.ReleaseAdmission("claude", ticket)
	allow, _, next := cb.Admit("claude")
	if !allow || !next.probe {
		t.Fatal("neutral abandoned probe held the lease")
	}
}

func TestBulkheadHalfOpenHasOneLiveProbeAcrossCooldown(t *testing.T) {
	cb := newProviderCircuitBreaker(1, time.Second)
	clock := time.Now()
	cb.now = func() time.Time { return clock }
	cb.RecordFailure("claude")
	clock = clock.Add(2 * time.Second)
	allow, _, old := cb.Admit("claude")
	if !allow {
		t.Fatal("first probe missing")
	}
	clock = clock.Add(time.Hour)
	for i := 0; i < 1000; i++ {
		if allow, _, _ := cb.Admit("claude"); allow {
			t.Fatal("cooldown allowed overlapping live probe")
		}
	}
	cb.ReleaseAdmission("claude", old)
	allow, _, current := cb.Admit("claude")
	if !allow {
		t.Fatal("released probe not rearmed")
	}
	cb.ReleaseAdmission("claude", old)
	cb.RecordAdmittedSuccess("claude", old)
	if allow, _, _ := cb.Admit("claude"); allow {
		t.Fatal("old probe cleared a new lease")
	}
	cb.RecordAdmittedSuccess("claude", current)
	if allow, _, _ := cb.Admit("claude"); !allow {
		t.Fatal("current lease did not close circuit")
	}
}
