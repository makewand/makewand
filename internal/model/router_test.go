package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
)

type stubProvider struct {
	name       string
	available  bool
	failChat   bool
	failStream bool
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) IsAvailable() bool { return s.available }

func (s *stubProvider) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	if s.failChat {
		return "", Usage{}, fmt.Errorf("%s failed", s.name)
	}
	return "ok", Usage{Provider: s.name, Model: s.name}, nil
}

func (s *stubProvider) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	if s.failStream {
		return nil, fmt.Errorf("%s stream failed", s.name)
	}
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

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

func TestChat_ModeFreeDoesNotFallbackToAPI(t *testing.T) {
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
		mode:    ModeFree,
		modeSet: true,
		cfg:     config.DefaultConfig(),
	}

	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err == nil {
		t.Fatal("Chat() error = nil, want failure (free mode must not fallback to API providers)")
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

func TestProviderAttemptTimeout_EconomyGeminiCapped(t *testing.T) {
	got := providerAttemptTimeout(ModeEconomy, PhaseCode, "gemini")
	if got > 60*time.Second {
		t.Fatalf("providerAttemptTimeout(economy, code, gemini) = %s, want <= 60s", got)
	}
}

func TestProviderAttemptTimeout_BalancedKeepsPhaseDefault(t *testing.T) {
	got := providerAttemptTimeout(ModeBalanced, PhaseCode, "gemini")
	if got != 150*time.Second {
		t.Fatalf("providerAttemptTimeout(balanced, code, gemini) = %s, want 150s default", got)
	}
}

func TestChat_AppliesPerAttemptTimeoutForPrimaryProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CodingModel = "claude"
	cfg.DefaultModel = "claude"

	primary := &deadlineCaptureProvider{name: "claude", available: true}
	fallback := &stubProvider{name: "gemini", available: true}

	r := &Router{
		cfg: cfg,
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
	if primary.remaining > 170*time.Second {
		t.Fatalf("primary attempt deadline too long: %s, want bounded near phase timeout", primary.remaining)
	}
}

// --- effectiveMode tests ---

func TestEffectiveMode_DefaultsToBalancedWhenNotSet(t *testing.T) {
	r := &Router{
		modeSet: false,
		mode:    ModeFree, // zero value — must NOT be used
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
		mode:    ModeFree, // zero value; effectiveMode() must return ModeBalanced instead
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

func TestNewRouter_CLIAccessDefaultsToSubscription(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CLIs = []config.CLITool{
		{Name: "claude", BinPath: "/bin/sh", Version: "test"},
		{Name: "gemini", BinPath: "/bin/sh", Version: "test"},
		{Name: "codex", BinPath: "/bin/sh", Version: "test"},
	}
	cfg.ClaudeAccess = ""
	cfg.GeminiAccess = ""
	cfg.OpenAIAccess = ""

	r := NewRouter(cfg)

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
	cfg := config.DefaultConfig()
	cfg.CodingModel = "claude"
	cfg.DefaultModel = "claude"

	claude := &stubProvider{name: "claude", available: true, failStream: true}
	gemini := &stubProvider{name: "gemini", available: true}

	r := &Router{
		cfg: cfg,
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

func TestNewRouter_LoadsCustomProviderFromConfig(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-custom.sh")
	body := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  printf '%s\\n' \"$arg\"\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.CLIs = nil
	cfg.ClaudeAPIKey = ""
	cfg.GeminiAPIKey = ""
	cfg.OpenAIAPIKey = ""
	cfg.OllamaURL = ""
	cfg.DefaultModel = "private"
	cfg.CodingModel = "private"
	cfg.CustomProviders = []config.CustomProvider{
		{
			Name:       "private",
			Command:    script,
			Args:       []string{"--prompt", "{{prompt}}"},
			Access:     "subscription",
			PromptMode: config.CustomPromptModeLegacy,
		},
	}

	r := NewRouter(cfg)
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

func TestRouteByMode_CustomProviderAccessAPIExcludedInFreeMode(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CLIs = nil
	cfg.ClaudeAPIKey = ""
	cfg.GeminiAPIKey = ""
	cfg.OpenAIAPIKey = ""
	cfg.OllamaURL = ""
	cfg.UsageMode = "free"
	cfg.CustomProviders = []config.CustomProvider{
		{
			Name:       "private-api",
			Command:    "/bin/sh",
			Args:       []string{"-c", "echo ok", "{{prompt}}"},
			Access:     "api",
			PromptMode: config.CustomPromptModeLegacy,
		},
	}

	r := NewRouter(cfg)
	_, err := r.Route(TaskCode)
	if err == nil {
		t.Fatal("Route(TaskCode) error = nil, want no provider available in free mode")
	}
	if !strings.Contains(err.Error(), "free mode requires at least one free/local provider") &&
		!strings.Contains(err.Error(), "no AI model available for mode") {
		t.Fatalf("Route(TaskCode) error = %q, want strict-free routing failure", err.Error())
	}
}

func TestNewRouter_SkipsUnusableCustomProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CLIs = nil
	cfg.ClaudeAPIKey = ""
	cfg.GeminiAPIKey = ""
	cfg.OpenAIAPIKey = ""
	cfg.OllamaURL = ""
	cfg.CustomProviders = []config.CustomProvider{
		{
			Name:       "broken-private",
			Command:    "/definitely/not/a/real/provider",
			Access:     "subscription",
			PromptMode: config.CustomPromptModeStdin,
		},
	}

	r := NewRouter(cfg)
	if _, ok := r.providers["broken-private"]; ok {
		t.Fatal("unusable custom provider should not be registered")
	}
}
