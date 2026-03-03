package model

import (
	"context"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
)

func TestProviderCircuitBreaker_StateTransitions(t *testing.T) {
	now := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)
	cb := newProviderCircuitBreaker(2, 10*time.Second)
	cb.now = func() time.Time { return now }

	if opened, _ := cb.RecordFailure("claude"); opened {
		t.Fatal("first failure should not open the circuit")
	}
	opened, until := cb.RecordFailure("claude")
	if !opened {
		t.Fatal("second failure should open the circuit")
	}
	if !until.After(now) {
		t.Fatal("open-until timestamp should be in the future")
	}

	if allow, _ := cb.BeforeAttempt("claude"); allow {
		t.Fatal("BeforeAttempt should block while circuit is open")
	}

	now = now.Add(11 * time.Second)
	if allow, _ := cb.BeforeAttempt("claude"); !allow {
		t.Fatal("BeforeAttempt should allow one half-open trial after cooldown")
	}
	if opened, _ := cb.RecordFailure("claude"); !opened {
		t.Fatal("failure in half-open should re-open the circuit immediately")
	}

	now = now.Add(11 * time.Second)
	if allow, _ := cb.BeforeAttempt("claude"); !allow {
		t.Fatal("second cooldown expiry should allow another half-open trial")
	}
	cb.RecordSuccess("claude")
	if blocked, _ := cb.PeekOpen("claude"); blocked {
		t.Fatal("RecordSuccess should close the circuit")
	}
}

func TestRoute_SkipsProviderWithOpenCircuit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CodingModel = "claude"
	cfg.DefaultModel = "claude"

	claude := &stubProvider{name: "claude", available: true}
	gemini := &stubProvider{name: "gemini", available: true}

	r := &Router{
		cfg: cfg,
		providers: map[string]Provider{
			"claude": claude,
			"gemini": gemini,
		},
		accessTypes: map[string]AccessType{
			"claude": AccessSubscription,
			"gemini": AccessFree,
		},
		providerCache: make(map[providerKey]Provider),
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(1, time.Hour),
	}

	r.breaker.RecordFailure("claude") // threshold=1 -> opens immediately

	res, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route(TaskCode) error: %v", err)
	}
	if res.Actual != "gemini" || !res.IsFallback {
		t.Fatalf("Route(TaskCode)=%+v, want fallback to gemini because claude circuit is open", res)
	}
}

func TestChat_FallbackWhenPrimaryCircuitOpen(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CodingModel = "claude"
	cfg.DefaultModel = "claude"

	claude := &stubProvider{name: "claude", available: true}
	gemini := &stubProvider{name: "gemini", available: true}

	r := &Router{
		cfg: cfg,
		providers: map[string]Provider{
			"claude": claude,
			"gemini": gemini,
		},
		accessTypes: map[string]AccessType{
			"claude": AccessSubscription,
			"gemini": AccessFree,
		},
		providerCache: make(map[providerKey]Provider),
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(1, time.Hour),
	}

	r.breaker.RecordFailure("claude")

	_, _, res, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if res.Actual != "gemini" || !res.IsFallback {
		t.Fatalf("Chat route=%+v, want fallback to gemini", res)
	}
}

func TestRegisterProviderResolver_ResolveCustomProvider(t *testing.T) {
	const customName = "custom-resolver-test"
	if err := RegisterProviderResolver(customName, func(cfg *config.Config, modelID string) (Provider, error) {
		return &stubProvider{name: customName, available: true}, nil
	}); err != nil {
		t.Fatalf("RegisterProviderResolver: %v", err)
	}

	r := NewRouter(config.DefaultConfig())
	p, err := r.resolveProvider(customName, "custom-model")
	if err != nil {
		t.Fatalf("resolveProvider(custom): %v", err)
	}
	if p.Name() != customName {
		t.Fatalf("resolved provider name=%q, want %q", p.Name(), customName)
	}
}

func TestRegisterProvider_RuntimeInjection(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CodingModel = "private"
	cfg.DefaultModel = "private"
	r := NewRouter(cfg)

	privateProv := &stubProvider{name: "private", available: true}
	if err := r.RegisterProvider("private", privateProv, AccessAPI); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	res, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route(TaskCode): %v", err)
	}
	if res.Actual != "private" {
		t.Fatalf("Route(TaskCode) actual=%q, want %q", res.Actual, "private")
	}
}

func TestRouteByMode_UsesDynamicallyRegisteredProvider(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	r.SetMode(ModeBalanced)

	privateProv := &stubProvider{name: "private", available: true}
	if err := r.RegisterProvider("private", privateProv, AccessSubscription); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	res, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route(TaskCode): %v", err)
	}
	if res.Actual != "private" {
		t.Fatalf("Route(TaskCode) actual=%q, want %q", res.Actual, "private")
	}
}

func TestRouteProvider_FallsBackToDynamicProvider(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	r.SetMode(ModeBalanced)

	privateProv := &stubProvider{name: "private", available: true}
	if err := r.RegisterProvider("private", privateProv, AccessSubscription); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	res, err := r.RouteProvider("claude", PhaseCode)
	if err != nil {
		t.Fatalf("RouteProvider: %v", err)
	}
	if res.Actual != "private" || !res.IsFallback {
		t.Fatalf("RouteProvider result=%+v, want fallback to private", res)
	}
}
