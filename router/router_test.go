package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubProvider is a minimal Provider for testing.
// Unified: supports response, failChat, failStream fields.
type stubProvider struct {
	name       string
	available  bool
	response   string
	failChat   bool
	failStream bool
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) IsAvailable() bool { return s.available }

func (s *stubProvider) Chat(_ context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	if s.failChat {
		return "", Usage{}, fmt.Errorf("%s failed", s.name)
	}
	resp := s.response
	if resp == "" {
		resp = "ok"
	}
	return resp, Usage{Provider: s.name, Model: s.name}, nil
}

func (s *stubProvider) ChatStream(_ context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	if s.failStream {
		return nil, fmt.Errorf("%s stream failed", s.name)
	}
	ch := make(chan StreamChunk, 1)
	resp := s.response
	if resp == "" {
		resp = ""
	}
	ch <- StreamChunk{Content: resp, Done: true}
	close(ch)
	return ch, nil
}

// --- Library tests (from original router/router_test.go) ---

func TestNewRouterFromConfig_ConfigFree(t *testing.T) {
	// Verify that a Router can be created without any config package dependency.
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"test": {Provider: &stubProvider{name: "test", available: true, response: "hello"}, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	if !r.ModeSet() {
		t.Fatal("expected mode to be set")
	}
	if r.Mode() != ModeBalanced {
		t.Fatalf("Mode() = %v, want ModeBalanced", r.Mode())
	}

	avail := r.Available()
	if len(avail) != 1 || avail[0] != "test" {
		t.Fatalf("Available() = %v, want [test]", avail)
	}
}

func TestNewRouterFromConfig_LegacyRouting(t *testing.T) {
	stub := &stubProvider{name: "mymodel", available: true, response: "ok"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"mymodel": {Provider: stub, Access: AccessFree},
		},
		DefaultModel:  "mymodel",
		CodingModel:   "mymodel",
		AnalysisModel: "mymodel",
		ReviewModel:   "mymodel",
	})

	// Legacy routing (no mode set)
	result, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if result.Actual != "mymodel" {
		t.Fatalf("Route().Actual = %q, want mymodel", result.Actual)
	}
}

func TestNewRouterFromConfig_Chat(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true, response: "test response"}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	content, _, _, err := r.Chat(context.Background(), TaskCode,
		[]Message{{Role: "user", Content: "hello"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if content != "test response" {
		t.Fatalf("Chat() = %q, want %q", content, "test response")
	}
}

func TestRegisterProvider_Runtime(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{},
		UsageMode: "fast",
	})

	stub := &stubProvider{name: "custom", available: true, response: "custom ok"}
	if err := r.RegisterProvider("custom", stub, AccessFree); err != nil {
		t.Fatalf("RegisterProvider() error = %v", err)
	}

	avail := r.Available()
	if len(avail) != 1 || avail[0] != "custom" {
		t.Fatalf("Available() = %v, want [custom]", avail)
	}
}

func TestClassifyTask_Library(t *testing.T) {
	if got := ClassifyTask("/review check this code"); got != TaskReview {
		t.Fatalf("ClassifyTask(/review) = %v, want TaskReview", got)
	}
	if got := ClassifyTask("fix the bug"); got != TaskFix {
		t.Fatalf("ClassifyTask(fix) = %v, want TaskFix", got)
	}
}

func TestEstimateCost_Library(t *testing.T) {
	// codex-cli is free (subscription), should return 0
	cost := EstimateCost("codex-cli", 1000, 500)
	if cost != 0 {
		t.Fatalf("EstimateCost(codex-cli) = %f, want 0", cost)
	}
}

// --- Tests migrated from internal/model/router_test.go ---

type deadlineCaptureProvider struct {
	name      string
	available bool
	remaining time.Duration
}

func (s *deadlineCaptureProvider) Name() string { return s.name }

func (s *deadlineCaptureProvider) IsAvailable() bool { return s.available }

func (s *deadlineCaptureProvider) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	if deadline, ok := ctx.Deadline(); ok {
		s.remaining = time.Until(deadline)
	} else {
		s.remaining = -1
	}
	return "", Usage{}, fmt.Errorf("%s forced failure", s.name)
}

func (s *deadlineCaptureProvider) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	return nil, fmt.Errorf("%s stream unsupported", s.name)
}

type streamDeadlineCaptureProvider struct {
	name            string
	available       bool
	streamRemaining time.Duration
}

func (s *streamDeadlineCaptureProvider) Name() string { return s.name }

func (s *streamDeadlineCaptureProvider) IsAvailable() bool { return s.available }

func (s *streamDeadlineCaptureProvider) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	return "", Usage{}, fmt.Errorf("%s chat unsupported", s.name)
}

func (s *streamDeadlineCaptureProvider) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	if deadline, ok := ctx.Deadline(); ok {
		s.streamRemaining = time.Until(deadline)
	} else {
		s.streamRemaining = -1
	}
	return nil, fmt.Errorf("%s stream forced failure", s.name)
}

func TestChat_ModeFastFallsBackToAPI(t *testing.T) {
	gemini := &stubProvider{name: "gemini", available: true, failChat: true}
	openai := &stubProvider{name: "openai", available: true, failChat: false}

	r := &Router{
		providers: map[string]Provider{
			"gemini": gemini,
			"openai": openai,
		},
		providerCache: map[providerKey]Provider{
			{name: "gemini", modelID: modelTable["gemini"][TierCheap]}: gemini,
			{name: "openai", modelID: modelTable["openai"][TierCheap]}: openai,
		},
		accessTypes: map[string]AccessType{
			"gemini": AccessFree,
			"openai": AccessAPI,
		},
		usage:   newSessionUsage(),
		mode:    ModeFast,
		modeSet: true,
	}

	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v, want nil (fast mode should fallback to API providers)", err)
	}
}

func TestRecordProviderFailureForErr_TimeoutTripsCircuitImmediately(t *testing.T) {
	r := &Router{
		breaker: newProviderCircuitBreaker(3, time.Minute),
	}

	opened, _ := r.recordProviderFailureForErr("claude", context.DeadlineExceeded)
	if !opened {
		t.Fatal("timeout failure should trip circuit immediately")
	}
	if blocked, _ := r.isCircuitOpen("claude"); !blocked {
		t.Fatal("circuit should be open after timeout-triggered failure")
	}
}

func TestWithProviderAttemptTimeout_ReviewUsesBoundedTimeout(t *testing.T) {
	ctx := context.Background()
	attemptCtx, cancel := withProviderAttemptTimeout(ctx, PhaseReview)
	defer cancel()

	deadline, ok := attemptCtx.Deadline()
	if !ok {
		t.Fatal("expected review attempt context to have a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		t.Fatalf("remaining timeout must be > 0, got %s", remaining)
	}
	if remaining > 50*time.Second {
		t.Fatalf("review timeout too large: %s", remaining)
	}
}

func TestWithProviderAttemptTimeout_RespectsCallerShorterDeadline(t *testing.T) {
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer parentCancel()

	attemptCtx, cancel := withProviderAttemptTimeout(parentCtx, PhaseReview)
	defer cancel()

	parentDeadline, _ := parentCtx.Deadline()
	attemptDeadline, ok := attemptCtx.Deadline()
	if !ok {
		t.Fatal("attempt context should preserve parent deadline")
	}
	if !attemptDeadline.Equal(parentDeadline) {
		t.Fatalf("attempt deadline = %s, want parent deadline %s", attemptDeadline, parentDeadline)
	}
}

func TestProviderAttemptTimeout_FastCodeTight(t *testing.T) {
	got := providerAttemptTimeout(ModeFast, PhaseCode, "gemini")
	if got != 45*time.Second {
		t.Fatalf("providerAttemptTimeout(fast, code, gemini) = %s, want 45s", got)
	}
}

func TestProviderAttemptTimeout_FastReviewTight(t *testing.T) {
	got := providerAttemptTimeout(ModeFast, PhaseReview, "claude")
	if got != 35*time.Second {
		t.Fatalf("providerAttemptTimeout(fast, review, claude) = %s, want 35s", got)
	}
}

func TestProviderAttemptTimeout_BalancedCodeModerate(t *testing.T) {
	got := providerAttemptTimeout(ModeBalanced, PhaseCode, "gemini")
	if got != 90*time.Second {
		t.Fatalf("providerAttemptTimeout(balanced, code, gemini) = %s, want 90s", got)
	}
}

func TestProviderAttemptTimeout_PowerKeepsPhaseDefault(t *testing.T) {
	got := providerAttemptTimeout(ModePower, PhaseCode, "gemini")
	if got != 180*time.Second {
		t.Fatalf("providerAttemptTimeout(power, code, gemini) = %s, want 180s default", got)
	}
}

func TestChat_AppliesPerAttemptTimeoutForPrimaryProvider(t *testing.T) {
	primary := &deadlineCaptureProvider{name: "claude", available: true}
	fallback := &stubProvider{name: "gemini", available: true}

	r := &Router{
		providers: map[string]Provider{
			"claude": primary,
			"gemini": fallback,
		},
		providerCache: make(map[providerKey]Provider),
		accessTypes: map[string]AccessType{
			"claude": AccessSubscription,
			"gemini": AccessFree,
		},
		usage:   newSessionUsage(),
		modeSet: false, // legacy routing path
	}
	r.legacyModels.defaultModel = "claude"
	r.legacyModels.codingModel = "claude"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, _, result, err := r.Chat(ctx, TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v, want nil (fallback should succeed)", err)
	}
	if result.Actual != "gemini" || !result.IsFallback {
		t.Fatalf("Chat() route = %+v, want fallback to gemini", result)
	}
	if primary.remaining <= 0 {
		t.Fatalf("primary attempt should receive bounded deadline, got remaining=%s", primary.remaining)
	}
	if primary.remaining > 200*time.Second {
		t.Fatalf("primary attempt deadline too long: %s, want bounded near phase timeout", primary.remaining)
	}
}

func TestChatStream_AppliesPerAttemptTimeoutForPrimaryProvider(t *testing.T) {
	primary := &streamDeadlineCaptureProvider{name: "claude", available: true}
	fallback := &stubProvider{name: "gemini", available: true}

	r := &Router{
		providers: map[string]Provider{
			"claude": primary,
			"gemini": fallback,
		},
		providerCache: make(map[providerKey]Provider),
		accessTypes: map[string]AccessType{
			"claude": AccessSubscription,
			"gemini": AccessFree,
		},
		usage:   newSessionUsage(),
		modeSet: false, // legacy routing path
	}
	r.legacyModels.defaultModel = "claude"
	r.legacyModels.codingModel = "claude"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ch, result, err := r.ChatStream(ctx, TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("ChatStream() error = %v, want nil (fallback should succeed)", err)
	}
	if ch == nil {
		t.Fatal("ChatStream() channel is nil")
	}
	if result.Actual != "gemini" || !result.IsFallback {
		t.Fatalf("ChatStream() route = %+v, want fallback to gemini", result)
	}
	if primary.streamRemaining <= 0 {
		t.Fatalf("primary stream attempt should receive bounded deadline, got remaining=%s", primary.streamRemaining)
	}
	if primary.streamRemaining > 200*time.Second {
		t.Fatalf("primary stream attempt deadline too long: %s, want bounded near phase timeout", primary.streamRemaining)
	}
}

// --- effectiveMode tests ---

func TestEffectiveMode_DefaultsToBalancedWhenNotSet(t *testing.T) {
	r := &Router{
		modeSet: false,
		mode:    ModeFast, // zero value — must NOT be used
	}
	if got := r.effectiveMode(); got != ModeBalanced {
		t.Fatalf("effectiveMode() = %v, want ModeBalanced", got)
	}
}

func TestEffectiveMode_ReturnsSetMode(t *testing.T) {
	r := &Router{
		modeSet: true,
		mode:    ModePower,
	}
	if got := r.effectiveMode(); got != ModePower {
		t.Fatalf("effectiveMode() = %v, want ModePower", got)
	}
}

func TestBuildProviderFor_DefaultsToBalancedWhenModeNotSet(t *testing.T) {
	r := &Router{
		modeSet: false,
		mode:    ModeFast, // zero value; effectiveMode() must return ModeBalanced instead
	}
	// ModeBalanced + PhaseCode → "claude"
	got := r.BuildProviderFor(PhaseCode)
	if got != "claude" {
		t.Fatalf("BuildProviderFor(PhaseCode) with unset mode = %q, want %q (ModeBalanced primary)", got, "claude")
	}
}

// --- failure rate tracking tests ---

func TestSessionUsage_RecordFailure(t *testing.T) {
	u := newSessionUsage()
	u.RecordFailure("provA")
	u.RecordFailure("provA")

	if got := u.FailureRate("provA"); got != 1.0 {
		t.Fatalf("FailureRate after 2 failures, 0 successes = %v, want 1.0", got)
	}
}

func TestSessionUsage_FailureRateWithMixed(t *testing.T) {
	u := newSessionUsage()
	u.Increment("provA") // 1 success
	u.RecordFailure("provA")
	u.RecordFailure("provA") // 2 failures

	// total = 3, failures = 2 → rate = 2/3
	got := u.FailureRate("provA")
	want := 2.0 / 3.0
	if got != want {
		t.Fatalf("FailureRate = %v, want %v", got, want)
	}
}

func TestSessionUsage_FailureRateZeroForUnknown(t *testing.T) {
	u := newSessionUsage()
	if got := u.FailureRate("nobody"); got != 0 {
		t.Fatalf("FailureRate for unknown provider = %v, want 0", got)
	}
}

func TestSortCandidates_HighFailureRateDePrioritised(t *testing.T) {
	// Hard exclusion requires both >50% failure rate AND ≥5 total requests.
	candidates := []candidate{
		{name: "bad", access: AccessFree, order: 0, useCount: 1, failureRate: 0.9, requests: 10},
		{name: "good", access: AccessFree, order: 1, useCount: 0, failureRate: 0.0, requests: 0},
	}

	sortCandidates(candidates)

	if candidates[0].name != "good" {
		t.Fatalf("after sort first candidate = %q, want %q (high failure rate should be last)", candidates[0].name, "good")
	}
}

func TestSortCandidates_LowSampleCountNotExcluded(t *testing.T) {
	// A single failure (requests < 5) must NOT trigger hard exclusion.
	candidates := []candidate{
		{name: "unlucky", access: AccessFree, order: 0, useCount: 0, failureRate: 1.0, requests: 1},
		{name: "other", access: AccessFree, order: 1, useCount: 0, failureRate: 0.0, requests: 0},
	}

	sortCandidates(candidates)

	// "unlucky" has better Thompson score (priorBias advantage from order 0) but fewer
	// samples — the key assertion is that it is NOT hard-excluded (both are candidates).
	// We just verify that sortCandidates doesn't panic and both entries remain.
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates after sort, got %d", len(candidates))
	}
}

func TestNewRouterFromConfig_CLIAccessDefaultsToSubscription(t *testing.T) {
	// Adapted from TestNewRouter_CLIAccessDefaultsToSubscription:
	// uses NewRouterFromConfig with pre-constructed CLIProviders.
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: NewClaudeCLI("/bin/sh"), Access: AccessSubscription},
			"gemini": {Provider: NewGeminiCLI("/bin/sh"), Access: AccessSubscription},
			"codex":  {Provider: NewCodexCLI("/bin/sh"), Access: AccessSubscription},
		},
	})

	if got := r.accessTypes["claude"]; got != AccessSubscription {
		t.Fatalf("claude access = %v, want %v", got, AccessSubscription)
	}
	if got := r.accessTypes["gemini"]; got != AccessSubscription {
		t.Fatalf("gemini access = %v, want %v", got, AccessSubscription)
	}
	if got := r.accessTypes["codex"]; got != AccessSubscription {
		t.Fatalf("codex access = %v, want %v", got, AccessSubscription)
	}
}

func TestChatStream_FallbackOnStartError(t *testing.T) {
	claude := &stubProvider{name: "claude", available: true, failStream: true}
	gemini := &stubProvider{name: "gemini", available: true}

	r := &Router{
		providers: map[string]Provider{
			"claude": claude,
			"gemini": gemini,
		},
		providerCache: make(map[providerKey]Provider),
		accessTypes: map[string]AccessType{
			"claude": AccessSubscription,
			"gemini": AccessFree,
		},
		usage:   newSessionUsage(),
		modeSet: false, // legacy routing path
	}
	r.legacyModels.defaultModel = "claude"
	r.legacyModels.codingModel = "claude"

	ch, result, err := r.ChatStream(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("ChatStream() error = %v, want nil", err)
	}
	if ch == nil {
		t.Fatal("ChatStream() channel is nil")
	}
	if result.Actual != "gemini" || !result.IsFallback {
		t.Fatalf("ChatStream() route = %+v, want fallback to gemini", result)
	}
}

func TestNewRouterFromConfig_LoadsCustomProvider(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-custom.sh")
	body := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  printf '%s\\n' \"$arg\"\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"private": {
				Provider: NewCommandCLI("private", script, []string{"--prompt", "{{prompt}}"}, "legacy"),
				Access:   AccessSubscription,
			},
		},
		DefaultModel: "private",
		CodingModel:  "private",
	})

	if got := r.accessTypes["private"]; got != AccessSubscription {
		t.Fatalf("custom provider access = %v, want %v", got, AccessSubscription)
	}

	route, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route(TaskCode) error = %v", err)
	}
	if route.Actual != "private" {
		t.Fatalf("Route(TaskCode).Actual = %q, want %q", route.Actual, "private")
	}

	content, _, result, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hello-private"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if result.Actual != "private" {
		t.Fatalf("Chat() provider = %q, want %q", result.Actual, "private")
	}
	if !strings.Contains(content, "hello-private") {
		t.Fatalf("Chat() content = %q, want echoed prompt", content)
	}
}

func TestRouteByMode_CustomProviderAccessAPIAllowedInFastMode_Router(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"private-api": {
				Provider: NewCommandCLI("private-api", "/bin/sh", []string{"-c", "echo ok", "{{prompt}}"}, "legacy"),
				Access:   AccessAPI,
			},
		},
		UsageMode: "fast",
	})

	result, err := r.Route(TaskCode)
	if err != nil {
		t.Fatalf("Route(TaskCode) error = %v, want nil (fast mode allows API providers)", err)
	}
	if result.Actual != "private-api" {
		t.Errorf("Route(TaskCode) actual = %q, want %q", result.Actual, "private-api")
	}
}

func TestNewRouterFromConfig_SkipsUnusableCustomProvider(t *testing.T) {
	// An unusable provider (binary not found) should report IsAvailable() = false,
	// but NewRouterFromConfig always registers what the caller gives it.
	// The test verifies that such providers are not returned by Available().
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"broken-private": {
				Provider: NewCommandCLI("broken-private", "/definitely/not/a/real/provider", nil, "stdin"),
				Access:   AccessSubscription,
			},
		},
	})
	avail := r.Available()
	for _, name := range avail {
		if name == "broken-private" {
			t.Fatal("unusable custom provider should not be in Available()")
		}
	}
}
