package router

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// recordingUnsafeProvider is UNSAFE (it does not implement UntrustedRepoCapable)
// and records every IsAvailable/Chat/ChatStream call, so tests can assert that
// untrusted mode never probes or executes it.
type recordingUnsafeProvider struct {
	name           string
	availableCalls atomic.Int32
	chatCalls      atomic.Int32
	streamCalls    atomic.Int32
}

func (p *recordingUnsafeProvider) Name() string { return p.name }

func (p *recordingUnsafeProvider) IsAvailable() bool {
	p.availableCalls.Add(1)
	return true
}

func (p *recordingUnsafeProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	p.chatCalls.Add(1)
	return p.name + "-output", Usage{Provider: p.name, Model: p.name}, nil
}

func (p *recordingUnsafeProvider) ChatStream(context.Context, []Message, string, int) (<-chan StreamChunk, error) {
	p.streamCalls.Add(1)
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: p.name + "-output", Done: true}
	close(ch)
	return ch, nil
}

func newUntrustedChatContext() *attemptContext {
	return &attemptContext{
		ctx:       context.Background(),
		messages:  []Message{{Role: "user", Content: "hi"}},
		maxTokens: 1024,
		mode:      ModeBalanced,
		requested: "claude",
		labels: attemptLabels{
			attemptSuccess:  "chat_attempt_success",
			attemptError:    "chat_attempt_error",
			fallbackSkipped: "chat_fallback_skipped",
			failedAll:       "chat_failed_all",
		},
	}
}

// (a) Unsafe primary + EMPTY fallbacks must fail closed at the execution
// pipeline, not return an empty-content success. This is the bug where
// iterateFallbackCandidates returned a nil error for an empty list, which
// routeAndExecute misread as "a fallback succeeded".
func TestUntrusted_RouteAndExecute_EmptyFallbacks_FailsClosed(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	r := newLegacyTrustRouter(unsafe)
	r.SetRepoTrust(RepoTrustUntrusted)

	result := RouteResult{Provider: unsafe, ModelID: "m", Requested: "claude", Actual: "claude"}

	content, _, _, err := r.routeAndExecute(newUntrustedChatContext(), result, nil, r.legacyResolver())
	if !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("routeAndExecute err = %v, want ErrNoUntrustedSafeProvider", err)
	}
	if content != "" {
		t.Fatalf("routeAndExecute content = %q, want empty (fail closed, no empty-success)", content)
	}
	if got := unsafe.chatCalls.Load(); got != 0 {
		t.Fatalf("primary executed %d times, want 0", got)
	}
}

func TestUntrusted_RouteAndExecuteStream_EmptyFallbacks_FailsClosed(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	r := newLegacyTrustRouter(unsafe)
	r.SetRepoTrust(RepoTrustUntrusted)

	result := RouteResult{Provider: unsafe, ModelID: "m", Requested: "claude", Actual: "claude"}

	ch, _, err := r.routeAndExecuteStream(newUntrustedChatContext(), result, nil, r.legacyResolver())
	if !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("routeAndExecuteStream err = %v, want ErrNoUntrustedSafeProvider", err)
	}
	if ch != nil {
		t.Fatalf("routeAndExecuteStream ch = %v, want nil (fail closed)", ch)
	}
	if got := unsafe.streamCalls.Load(); got != 0 {
		t.Fatalf("primary streamed %d times, want 0", got)
	}
}

// (b) A provider registered under the name "remote" that is a non-capable CLI
// must not bypass the capability gate via the remote-only fast path.
func TestUntrusted_RemoteOnlyNonCapable_RouteFailsClosed(t *testing.T) {
	remote := &recordingUnsafeProvider{name: "remote"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"remote": {Provider: remote, Access: AccessSubscription},
		},
		UsageMode: "balanced",
		RepoTrust: RepoTrustUntrusted,
	})

	if _, err := r.Route(TaskCode); !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("Route err = %v, want ErrNoUntrustedSafeProvider (remote fast path must not bypass capability gate)", err)
	}
	if got := remote.availableCalls.Load(); got != 0 {
		t.Fatalf("remote IsAvailable called %d times, want 0 (unsafe remote must not be probed)", got)
	}
}

// (d) Trusted mode: the remote-only fast path still returns the remote provider,
// probing it exactly once, so trusted behavior is unchanged.
func TestTrusted_RemoteOnly_RouteUsesFastPath(t *testing.T) {
	remote := &recordingUnsafeProvider{name: "remote"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"remote": {Provider: remote, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	res, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route err = %v, want nil (trusted remote fast path)", err)
	}
	if res.Actual != "remote" {
		t.Fatalf("Route Actual = %q, want remote", res.Actual)
	}
	if got := remote.availableCalls.Load(); got != 1 {
		t.Fatalf("remote IsAvailable called %d times, want 1 (trusted fast path probes once)", got)
	}
}

// (c) Legacy route must not probe (IsAvailable → exec) an unsafe provider in
// untrusted mode; the safety check runs before the availability probe.
func TestUntrusted_LegacyRoute_DoesNotProbeUnsafe(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	r := newLegacyTrustRouter(unsafe)
	r.SetRepoTrust(RepoTrustUntrusted)

	if _, err := r.Route(TaskCode); !errors.Is(err, ErrNoUntrustedSafeProvider) {
		t.Fatalf("legacy Route err = %v, want ErrNoUntrustedSafeProvider", err)
	}
	if got := unsafe.availableCalls.Load(); got != 0 {
		t.Fatalf("legacy route probed unsafe provider %d times, want 0", got)
	}
}

// (d) Trusted legacy route still probes and routes to the (unsafe-in-untrusted)
// provider — byte-for-byte unchanged.
func TestTrusted_LegacyRoute_ProbesAndRoutes(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	r := newLegacyTrustRouter(unsafe) // default trusted

	res, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("legacy Route err = %v, want nil (trusted)", err)
	}
	if res.Actual != "claude" {
		t.Fatalf("legacy Route Actual = %q, want claude", res.Actual)
	}
	if got := unsafe.availableCalls.Load(); got < 1 {
		t.Fatalf("trusted legacy route probed provider %d times, want >= 1", got)
	}
}

// (c) Available() must exclude unsafe providers without probing them in untrusted
// mode, while still listing safe ones.
func TestUntrusted_Available_DoesNotProbeUnsafe(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	safe := &capProvider{name: "openai", safe: true}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessSubscription},
			"openai": {Provider: safe, Access: AccessAPI},
		},
		UsageMode: "balanced",
		RepoTrust: RepoTrustUntrusted,
	})

	avail := r.Available()
	if got := unsafe.availableCalls.Load(); got != 0 {
		t.Fatalf("Available probed unsafe provider %d times, want 0", got)
	}
	foundOpenAI, foundClaude := false, false
	for _, n := range avail {
		switch n {
		case "openai":
			foundOpenAI = true
		case "claude":
			foundClaude = true
		}
	}
	if !foundOpenAI {
		t.Fatalf("Available = %v, want openai present (safe)", avail)
	}
	if foundClaude {
		t.Fatalf("Available = %v, want claude excluded (unsafe)", avail)
	}
}

// (d) Trusted Available() probes and lists the unsafe-in-untrusted provider.
func TestTrusted_Available_ProbesAll(t *testing.T) {
	unsafe := &recordingUnsafeProvider{name: "claude"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: unsafe, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	avail := r.Available()
	if got := unsafe.availableCalls.Load(); got < 1 {
		t.Fatalf("trusted Available probed provider %d times, want >= 1", got)
	}
	found := false
	for _, n := range avail {
		if n == "claude" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trusted Available = %v, want claude present", avail)
	}
}

// (c) The Power-mode judge must not be probed/executed when it is unsafe in
// untrusted mode. Generators are safe and produce content; the unsafe judge is
// skipped and the first result is returned (existing no-judge path).
func TestUntrusted_PowerJudge_DoesNotProbeUnsafeJudge(t *testing.T) {
	genA := &capProvider{name: "claude", safe: true}
	genB := &capProvider{name: "codex", safe: true}
	judge := &recordingUnsafeProvider{name: "gemini"} // PhaseCode judge is "gemini"
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: genA, Access: AccessAPI},
			"codex":  {Provider: genB, Access: AccessAPI},
			"gemini": {Provider: judge, Access: AccessSubscription},
		},
		UsageMode: "power",
		RepoTrust: RepoTrustUntrusted,
	})

	content, _, _, err := r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("ChatBest err = %v, want nil (safe generators produced content)", err)
	}
	if content != "claude-output" && content != "codex-output" {
		t.Fatalf("ChatBest content = %q, want a safe generator's output", content)
	}
	if got := judge.availableCalls.Load(); got != 0 {
		t.Fatalf("unsafe judge probed %d times, want 0", got)
	}
	if got := judge.chatCalls.Load(); got != 0 {
		t.Fatalf("unsafe judge executed %d times, want 0", got)
	}
}

// (d) Trusted Power judge is probed and executed as before.
func TestTrusted_PowerJudge_ProbesJudge(t *testing.T) {
	genA := &capProvider{name: "claude", safe: true}
	genB := &capProvider{name: "codex", safe: true}
	judge := &recordingUnsafeProvider{name: "gemini"}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: genA, Access: AccessAPI},
			"codex":  {Provider: genB, Access: AccessAPI},
			"gemini": {Provider: judge, Access: AccessSubscription},
		},
		UsageMode: "power",
	})

	if _, _, _, err := r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "hi"}}, ""); err != nil {
		t.Fatalf("ChatBest err = %v, want nil", err)
	}
	if got := judge.availableCalls.Load(); got < 1 {
		t.Fatalf("trusted judge probed %d times, want >= 1", got)
	}
}

// --- Quota refresh: never exec a local CLI to probe quota in untrusted mode. ---

// recordingQuotaSource records Read calls and can declare that reading it execs
// a local CLI (like agyQuotaSource).
type recordingQuotaSource struct {
	provider string
	execs    bool
	reads    atomic.Int32
}

func (s *recordingQuotaSource) Provider() string    { return s.provider }
func (s *recordingQuotaSource) execsLocalCLI() bool { return s.execs }
func (s *recordingQuotaSource) Read(context.Context) (ProviderQuota, error) {
	s.reads.Add(1)
	return ProviderQuota{Provider: s.provider, HasData: true, Authed: true}, nil
}

func TestQuotaRefresh_UntrustedSkipsLocalCLIProbe(t *testing.T) {
	execSrc := &recordingQuotaSource{provider: "gemini", execs: true}
	safeSrc := &recordingQuotaSource{provider: "claude", execs: false}
	s := NewQuotaSnapshotter(time.Hour, execSrc, safeSrc)
	s.SetRepoTrust(RepoTrustUntrusted)

	s.Refresh(context.Background())

	if got := execSrc.reads.Load(); got != 0 {
		t.Fatalf("untrusted refresh read exec source %d times, want 0 (must not exec local CLI)", got)
	}
	if got := safeSrc.reads.Load(); got != 1 {
		t.Fatalf("untrusted refresh read safe source %d times, want 1", got)
	}
}

func TestQuotaRefresh_TrustedProbesAllSources(t *testing.T) {
	execSrc := &recordingQuotaSource{provider: "gemini", execs: true}
	safeSrc := &recordingQuotaSource{provider: "claude", execs: false}
	s := NewQuotaSnapshotter(time.Hour, execSrc, safeSrc) // default trusted

	s.Refresh(context.Background())

	if got := execSrc.reads.Load(); got != 1 {
		t.Fatalf("trusted refresh read exec source %d times, want 1 (byte-for-byte unchanged)", got)
	}
	if got := safeSrc.reads.Load(); got != 1 {
		t.Fatalf("trusted refresh read safe source %d times, want 1", got)
	}
}

// NewRouterFromConfig must propagate its trust level into a concrete quota
// snapshotter so its refresh honors untrusted mode.
func TestNewRouter_PropagatesTrustToQuotaSnapshotter(t *testing.T) {
	execSrc := &recordingQuotaSource{provider: "gemini", execs: true}
	s := NewQuotaSnapshotter(time.Hour, execSrc)
	_ = mustNewRouter(RouterConfig{
		Quota:     s,
		RepoTrust: RepoTrustUntrusted,
		UsageMode: "balanced",
	})

	s.Refresh(context.Background())
	if got := execSrc.reads.Load(); got != 0 {
		t.Fatalf("router did not propagate untrusted trust: exec source read %d times, want 0", got)
	}
}
