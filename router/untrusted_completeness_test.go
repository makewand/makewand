package router

import (
	"context"
	"testing"
	"time"
)

// --- helpers ---

// newLegacyTrustRouter2 builds a legacy-routing Router (no mode set) with a
// primary registered under "claude" and a legacy fallback candidate under
// "codex", so tests can exercise the fallback-list construction path that
// Chat/ChatStream run before executing the primary.
func newLegacyTrustRouter2(primary, fallback Provider) *Router {
	r := &Router{
		providers:     map[string]Provider{"claude": primary, "codex": fallback},
		accessTypes:   map[string]AccessType{"claude": AccessAPI, "codex": AccessSubscription},
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

// hasProvider reports whether name is present in a provider-name slice.
func hasProvider(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// drainStream consumes a stream channel so the observer goroutine terminates.
func drainStream(ch <-chan StreamChunk) {
	if ch == nil {
		return
	}
	for range ch { //nolint:revive // intentionally draining
	}
}

// --- Gap 1: legacy Chat/ChatStream fallback-list construction must not probe an
// unsafe CLI (IsAvailable execs a host health check) in untrusted mode, even when
// the primary is a safe API provider that succeeds. ---

func TestUntrusted_LegacyChatFallbackList_DoesNotProbeUnsafe(t *testing.T) {
	safe := &capProvider{name: "claude", safe: true}
	unsafe := &recordingUnsafeProvider{name: "codex"}
	r := newLegacyTrustRouter2(safe, unsafe)
	r.SetRepoTrust(RepoTrustUntrusted)

	content, _, route, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("Chat err = %v, want nil (safe primary succeeds)", err)
	}
	if content != "claude-output" || route.Actual != "claude" {
		t.Fatalf("Chat content=%q actual=%q, want claude-output/claude", content, route.Actual)
	}
	if got := unsafe.availableCalls.Load(); got != 0 {
		t.Fatalf("unsafe fallback IsAvailable called %d times building the fallback list, want 0", got)
	}
	if got := unsafe.chatCalls.Load(); got != 0 {
		t.Fatalf("unsafe fallback executed %d times, want 0", got)
	}
}

func TestUntrusted_LegacyChatStreamFallbackList_DoesNotProbeUnsafe(t *testing.T) {
	safe := &capProvider{name: "claude", safe: true}
	unsafe := &recordingUnsafeProvider{name: "codex"}
	r := newLegacyTrustRouter2(safe, unsafe)
	r.SetRepoTrust(RepoTrustUntrusted)

	ch, route, err := r.ChatStream(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("ChatStream err = %v, want nil (safe primary streams)", err)
	}
	if route.Actual != "claude" {
		t.Fatalf("ChatStream route.Actual = %q, want claude", route.Actual)
	}
	drainStream(ch)
	if got := unsafe.availableCalls.Load(); got != 0 {
		t.Fatalf("unsafe fallback IsAvailable called %d times building the stream fallback list, want 0", got)
	}
	if got := unsafe.streamCalls.Load(); got != 0 {
		t.Fatalf("unsafe fallback streamed %d times, want 0", got)
	}
}

// Trusted mode: the fallback-list construction still probes the (unsafe-in-
// untrusted) provider — byte-for-byte unchanged.
func TestTrusted_LegacyChatFallbackList_ProbesUnsafe(t *testing.T) {
	safe := &capProvider{name: "claude", safe: true}
	unsafe := &recordingUnsafeProvider{name: "codex"}
	r := newLegacyTrustRouter2(safe, unsafe) // default trusted

	if _, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, ""); err != nil {
		t.Fatalf("Chat err = %v, want nil", err)
	}
	if got := unsafe.availableCalls.Load(); got < 1 {
		t.Fatalf("trusted fallback-list construction probed unsafe fallback %d times, want >= 1 (unchanged)", got)
	}
}

func TestTrusted_LegacyChatStreamFallbackList_ProbesUnsafe(t *testing.T) {
	safe := &capProvider{name: "claude", safe: true}
	unsafe := &recordingUnsafeProvider{name: "codex"}
	r := newLegacyTrustRouter2(safe, unsafe) // default trusted

	ch, _, err := r.ChatStream(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("ChatStream err = %v, want nil", err)
	}
	drainStream(ch)
	if got := unsafe.availableCalls.Load(); got < 1 {
		t.Fatalf("trusted stream fallback-list construction probed unsafe fallback %d times, want >= 1 (unchanged)", got)
	}
}

// --- Gap 2: Router.Get must not return (nor probe) an unsafe provider in
// untrusted mode; it fails closed with the not-found contract. ---

func TestUntrusted_Get_ReturnsNotFoundForUnsafe_WithoutProbing(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessSubscription},
		},
		UsageMode: "balanced",
		RepoTrust: RepoTrustUntrusted,
	})

	p, err := r.Get("claude")
	if err == nil {
		t.Fatalf("Get returned provider %v with nil error, want not-found error (unsafe in untrusted mode)", p)
	}
	if p != nil {
		t.Fatalf("Get returned non-nil provider %v, want nil", p)
	}
	if got := unsafe.availableCalls.Load(); got != 0 {
		t.Fatalf("Get probed unsafe provider %d times, want 0", got)
	}
}

func TestUntrusted_Get_ReturnsSafeProvider(t *testing.T) {
	safe := &capProvider{name: "openai", safe: true}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"openai": {Provider: safe, Access: AccessAPI},
		},
		UsageMode: "balanced",
		RepoTrust: RepoTrustUntrusted,
	})

	p, err := r.Get("openai")
	if err != nil {
		t.Fatalf("Get(openai) err = %v, want nil (safe provider allowed in untrusted mode)", err)
	}
	if p == nil {
		t.Fatalf("Get(openai) returned nil, want the safe provider")
	}
}

func TestTrusted_Get_ReturnsAndProbesUnsafe(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	p, err := r.Get("claude")
	if err != nil {
		t.Fatalf("Get err = %v, want nil (trusted)", err)
	}
	if p == nil {
		t.Fatalf("Get returned nil provider, want the registered provider")
	}
	if got := unsafe.availableCalls.Load(); got < 1 {
		t.Fatalf("trusted Get probed provider %d times, want >= 1 (unchanged)", got)
	}
}

// --- Gap 3: SetRepoTrust(Untrusted) after construction must invalidate the
// availability cache and propagate to the concrete quota snapshotter. ---

func TestSetRepoTrust_Untrusted_InvalidatesAvailabilityCache(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	safe := &capProvider{name: "openai", safe: true}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessSubscription},
			"openai": {Provider: safe, Access: AccessAPI},
		},
		UsageMode: "balanced",
	})

	// Trusted first: caches both providers as available (probing the unsafe one).
	before := r.Available()
	if !hasProvider(before, "claude") {
		t.Fatalf("trusted Available = %v, want claude present (cached)", before)
	}

	// Switch to untrusted. This must invalidate the availability cache so the next
	// Available() re-probes and drops the now-unsafe provider (rather than serving
	// the stale trusted cache within availCacheTTL).
	r.SetRepoTrust(RepoTrustUntrusted)

	after := r.Available()
	if hasProvider(after, "claude") {
		t.Fatalf("untrusted Available = %v, want claude excluded after SetRepoTrust invalidated the cache", after)
	}
	if !hasProvider(after, "openai") {
		t.Fatalf("untrusted Available = %v, want openai still present (safe)", after)
	}
}

func TestSetRepoTrust_Untrusted_PropagatesToSnapshotter(t *testing.T) {
	execSrc := &recordingQuotaSource{provider: "gemini", execs: true}
	safeSrc := &recordingQuotaSource{provider: "claude", execs: false}
	s := NewQuotaSnapshotter(time.Hour, execSrc, safeSrc)
	r := mustNewRouter(RouterConfig{
		Quota:     s,
		UsageMode: "balanced",
	}) // constructed trusted

	// A trusted refresh reads the local-CLI (exec) source.
	s.Refresh(context.Background())
	if got := execSrc.reads.Load(); got != 1 {
		t.Fatalf("trusted refresh read exec source %d times, want 1", got)
	}

	// Flip the router to untrusted AFTER construction; SetRepoTrust must reach the
	// already-constructed snapshotter so its next refresh skips the local CLI probe.
	r.SetRepoTrust(RepoTrustUntrusted)

	s.Refresh(context.Background())
	if got := execSrc.reads.Load(); got != 1 {
		t.Fatalf("after SetRepoTrust(untrusted), exec source read %d times total, want 1 (must skip local CLI quota probe)", got)
	}
	if got := safeSrc.reads.Load(); got != 2 {
		t.Fatalf("safe source read %d times total, want 2 (read on both refreshes)", got)
	}
}
