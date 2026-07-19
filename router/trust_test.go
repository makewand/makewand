package router

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// --- Test fake providers ---

// noCapProvider is a Provider that does NOT implement UntrustedRepoCapable, so it
// is treated as UNSAFE in untrusted mode (fail closed). It records how many times
// Chat/ChatStream were invoked so tests can assert "nothing executed".
type noCapProvider struct {
	name  string
	calls atomic.Int32
}

func (p *noCapProvider) Name() string      { return p.name }
func (p *noCapProvider) IsAvailable() bool { return true }

func (p *noCapProvider) Chat(_ context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	p.calls.Add(1)
	return p.name + "-output", Usage{Provider: p.name, Model: p.name}, nil
}

func (p *noCapProvider) ChatStream(_ context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	p.calls.Add(1)
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: p.name + "-output", Done: true}
	close(ch)
	return ch, nil
}

// capProvider implements UntrustedRepoCapable with a configurable safety verdict.
type capProvider struct {
	name  string
	safe  bool
	calls atomic.Int32
}

func (p *capProvider) Name() string               { return p.name }
func (p *capProvider) IsAvailable() bool          { return true }
func (p *capProvider) SafeForUntrustedRepo() bool { return p.safe }

func (p *capProvider) Chat(_ context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	p.calls.Add(1)
	return p.name + "-output", Usage{Provider: p.name, Model: p.name}, nil
}

func (p *capProvider) ChatStream(_ context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	p.calls.Add(1)
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: p.name + "-output", Done: true}
	close(ch)
	return ch, nil
}

// newLegacyTrustRouter builds a legacy-routing Router with a single provider
// registered under "claude" for the code/default tasks.
func newLegacyTrustRouter(p Provider) *Router {
	r := &Router{
		providers:     map[string]Provider{"claude": p},
		accessTypes:   map[string]AccessType{"claude": AccessSubscription},
		providerCache: make(map[providerKey]Provider),
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(defaultCircuitFailureThreshold, defaultCircuitCooldown),
	}
	r.legacyModels.defaultModel = "claude"
	r.legacyModels.codingModel = "claude"
	r.legacyModels.analysisModel = "claude"
	r.legacyModels.reviewModel = "claude"
	return r
}

// --- (a) SafeForUntrustedRepo per provider type ---

func TestSafeForUntrustedRepo_ByProviderType(t *testing.T) {
	safe := []Provider{
		NewClaude("key", ""),
		NewGemini("key", ""),
		NewOpenAI("key", ""),
		NewRemoteHTTP("https://remote.example", "token"),
	}
	for _, p := range safe {
		capable, ok := p.(UntrustedRepoCapable)
		if !ok {
			t.Fatalf("%s: does not implement UntrustedRepoCapable, want implemented", p.Name())
		}
		if !capable.SafeForUntrustedRepo() {
			t.Fatalf("%s: SafeForUntrustedRepo() = false, want true", p.Name())
		}
		if !providerSafeForUntrustedRepo(p) {
			t.Fatalf("%s: providerSafeForUntrustedRepo() = false, want true", p.Name())
		}
	}

	unsafe := []Provider{
		NewClaudeCLI("claude"),
		NewGeminiCLI("gemini"),
		NewCodexCLI("codex"),
		NewAgyCLI("agy"),
		NewCommandCLI("custom", "mytool", []string{"--flag"}, PromptModeArg),
	}
	for _, p := range unsafe {
		capable, ok := p.(UntrustedRepoCapable)
		if !ok {
			t.Fatalf("%s: does not implement UntrustedRepoCapable, want implemented (returning false)", p.Name())
		}
		if capable.SafeForUntrustedRepo() {
			t.Fatalf("%s: SafeForUntrustedRepo() = true, want false (CLI execs a host agent)", p.Name())
		}
		if providerSafeForUntrustedRepo(p) {
			t.Fatalf("%s: providerSafeForUntrustedRepo() = true, want false", p.Name())
		}
	}

	// A provider that omits the interface entirely is treated as unsafe.
	if providerSafeForUntrustedRepo(&noCapProvider{name: "x"}) {
		t.Fatal("provider without UntrustedRepoCapable should be treated as unsafe")
	}
}

// --- (b) Untrusted mode + only unsafe provider → fail closed, nothing executed ---

func TestUntrustedMode_FailsClosed_LegacyRouteAndChat(t *testing.T) {
	unsafe := &noCapProvider{name: "claude"}
	r := newLegacyTrustRouter(unsafe)
	r.SetRepoTrust(RepoTrustUntrusted)

	if _, err := r.Route(TaskCode); !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("Route(TaskCode) err = %v, want ErrNoUntrustedSafeProvider", err)
	}

	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("Chat err = %v, want ErrNoUntrustedSafeProvider", err)
	}
	if got := unsafe.calls.Load(); got != 0 {
		t.Fatalf("unsafe provider executed %d times, want 0 (must not execute in untrusted mode)", got)
	}

	// ChatStream must also fail closed without executing.
	_, _, streamErr := r.ChatStream(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if !errors.Is(streamErr, ErrNoUntrustedSafeProvider) {
		t.Fatalf("ChatStream err = %v, want ErrNoUntrustedSafeProvider", streamErr)
	}
	if got := unsafe.calls.Load(); got != 0 {
		t.Fatalf("unsafe provider executed %d times after ChatStream, want 0", got)
	}
}

// --- (c) Untrusted mode + safe provider available → routes normally ---

func TestUntrustedMode_RoutesToSafeProvider(t *testing.T) {
	safe := &capProvider{name: "claude", safe: true}
	r := newLegacyTrustRouter(safe)
	r.SetRepoTrust(RepoTrustUntrusted)

	res, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route(TaskCode) err = %v, want nil", err)
	}
	if res.Actual != "claude" {
		t.Fatalf("Route(TaskCode).Actual = %q, want claude", res.Actual)
	}

	content, _, route, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("Chat err = %v, want nil", err)
	}
	if content != "claude-output" {
		t.Fatalf("Chat content = %q, want claude-output", content)
	}
	if route.Actual != "claude" {
		t.Fatalf("Chat route.Actual = %q, want claude", route.Actual)
	}
	if got := safe.calls.Load(); got != 1 {
		t.Fatalf("safe provider executed %d times, want 1", got)
	}
}

// --- (d) Trusted mode (default) unchanged: a CLI-like unsafe provider still routes ---

func TestTrustedMode_UnsafeProviderStillRoutes(t *testing.T) {
	unsafe := &noCapProvider{name: "claude"}
	r := newLegacyTrustRouter(unsafe) // default trust = RepoTrustTrusted

	if r.RepoTrust() != RepoTrustTrusted {
		t.Fatalf("default RepoTrust = %v, want trusted", r.RepoTrust())
	}

	res, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route(TaskCode) err = %v, want nil (trusted mode unchanged)", err)
	}
	if res.Actual != "claude" {
		t.Fatalf("Route(TaskCode).Actual = %q, want claude", res.Actual)
	}

	content, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("Chat err = %v, want nil", err)
	}
	if content != "claude-output" {
		t.Fatalf("Chat content = %q, want claude-output", content)
	}
	if got := unsafe.calls.Load(); got != 1 {
		t.Fatalf("unsafe provider executed %d times in trusted mode, want 1", got)
	}
}

// --- Mode routing + RouteProvider + ChatBest fail-closed coverage ---

func TestUntrustedMode_ModeRouting_FailsClosed(t *testing.T) {
	unsafe := &noCapProvider{name: "claude"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessAPI},
		},
		UsageMode: "balanced",
		RepoTrust: RepoTrustUntrusted,
	})

	if _, err := r.Route(TaskCode); !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("mode Route err = %v, want ErrNoUntrustedSafeProvider", err)
	}

	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("mode Chat err = %v, want ErrNoUntrustedSafeProvider", err)
	}
	if got := unsafe.calls.Load(); got != 0 {
		t.Fatalf("unsafe provider executed %d times in untrusted mode routing, want 0", got)
	}
}

func TestUntrustedMode_RouteProvider_FailsClosed(t *testing.T) {
	unsafe := &noCapProvider{name: "claude"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessAPI},
		},
		UsageMode: "balanced",
		RepoTrust: RepoTrustUntrusted,
	})

	if _, err := r.RouteProvider("claude", PhaseCode); !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("RouteProvider err = %v, want ErrNoUntrustedSafeProvider", err)
	}

	_, _, _, err := r.ChatWith(context.Background(), "claude", PhaseCode, []Message{{Role: "user", Content: "hi"}}, "")
	if !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("ChatWith err = %v, want ErrNoUntrustedSafeProvider", err)
	}
	if got := unsafe.calls.Load(); got != 0 {
		t.Fatalf("unsafe provider executed %d times via ChatWith, want 0", got)
	}
}

func TestUntrustedMode_RouteProvider_RoutesToSafe(t *testing.T) {
	safe := &capProvider{name: "claude", safe: true}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: safe, Access: AccessAPI},
		},
		UsageMode: "balanced",
		RepoTrust: RepoTrustUntrusted,
	})

	content, _, route, err := r.ChatWith(context.Background(), "claude", PhaseCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("ChatWith err = %v, want nil", err)
	}
	if content != "claude-output" || route.Actual != "claude" {
		t.Fatalf("ChatWith content=%q actual=%q, want claude-output/claude", content, route.Actual)
	}
}

func TestUntrustedMode_ChatBestPower_FailsClosed(t *testing.T) {
	unsafe := &noCapProvider{name: "claude"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessAPI},
		},
		UsageMode: "power",
		RepoTrust: RepoTrustUntrusted,
	})

	_, _, _, err := r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "hi"}}, "")
	if !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("ChatBest(power) err = %v, want ErrNoUntrustedSafeProvider", err)
	}
	if got := unsafe.calls.Load(); got != 0 {
		t.Fatalf("unsafe provider executed %d times in power ensemble, want 0", got)
	}
}

// --- Accessors, config wiring, and clone inheritance ---

func TestRepoTrust_AccessorsAndConfig(t *testing.T) {
	r := mustNewRouter(RouterConfig{RepoTrust: RepoTrustUntrusted})
	if r.RepoTrust() != RepoTrustUntrusted {
		t.Fatalf("RepoTrust() = %v, want untrusted (from RouterConfig)", r.RepoTrust())
	}
	r.SetRepoTrust(RepoTrustTrusted)
	if r.RepoTrust() != RepoTrustTrusted {
		t.Fatalf("after SetRepoTrust: RepoTrust() = %v, want trusted", r.RepoTrust())
	}

	// Default (zero-value) RouterConfig yields trusted.
	if def := mustNewRouter(RouterConfig{}); def.RepoTrust() != RepoTrustTrusted {
		t.Fatalf("default RepoTrust() = %v, want trusted", def.RepoTrust())
	}
}

func TestRepoTrust_ClonePreservesTrust(t *testing.T) {
	r := mustNewRouter(RouterConfig{RepoTrust: RepoTrustUntrusted})
	clone := r.cloneWithMode(ModeBalanced)
	if clone.RepoTrust() != RepoTrustUntrusted {
		t.Fatalf("clone RepoTrust() = %v, want untrusted (must survive per-request clone)", clone.RepoTrust())
	}
}

// --- (e) ParseRepoTrust / String round-trip ---

func TestParseRepoTrust_And_String(t *testing.T) {
	for _, tr := range []RepoTrust{RepoTrustTrusted, RepoTrustUntrusted} {
		got, ok := ParseRepoTrust(tr.String())
		if !ok || got != tr {
			t.Fatalf("round-trip %v: ParseRepoTrust(%q) = (%v, %v), want (%v, true)", tr, tr.String(), got, ok, tr)
		}
	}

	cases := []struct {
		in     string
		want   RepoTrust
		wantOK bool
	}{
		{"trusted", RepoTrustTrusted, true},
		{"UNTRUSTED", RepoTrustUntrusted, true},
		{"  Untrusted  ", RepoTrustUntrusted, true},
		{"", RepoTrustTrusted, true},       // empty → default trusted, valid
		{"bogus", RepoTrustTrusted, false}, // unrecognized → trusted, not valid
	}
	for _, c := range cases {
		got, ok := ParseRepoTrust(c.in)
		if got != c.want || ok != c.wantOK {
			t.Fatalf("ParseRepoTrust(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}

	if RepoTrustTrusted.String() != "trusted" || RepoTrustUntrusted.String() != "untrusted" {
		t.Fatalf("String() mismatch: trusted=%q untrusted=%q", RepoTrustTrusted.String(), RepoTrustUntrusted.String())
	}
}

// Guard against accidental blocking-forever behavior in the fail-closed paths.
func TestUntrustedMode_FailsClosed_Fast(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := newLegacyTrustRouter(&noCapProvider{name: "claude"})
		r.SetRepoTrust(RepoTrustUntrusted)
		_, _, _, _ = r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("untrusted fail-closed Chat did not return promptly")
	}
}
