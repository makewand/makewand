package model

import (
	"context"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
)

// makeRouter creates a Router struct literal with stub providers wired for the given mode.
// provs maps provider name → stubProvider; access types are inferred from defaults
// unless overridden by the caller.
func makeRouter(t *testing.T, mode UsageMode, provs map[string]*stubProvider) *Router {
	t.Helper()

	providers := make(map[string]Provider, len(provs))
	cache := make(map[providerKey]Provider)
	access := make(map[string]AccessType)

	for name, stub := range provs {
		providers[name] = stub
		// Cache entries for all tiers so resolveProvider never hits a real resolver.
		for _, tier := range []ModelTier{TierCheap, TierMid, TierPremium} {
			if models, ok := modelTable[name]; ok {
				cache[providerKey{name: name, modelID: models[tier]}] = stub
			}
		}
		// Default access types mirror production defaults.
		switch name {
		case "gemini":
			access[name] = AccessFree
		case "ollama":
			access[name] = AccessLocal
		default:
			access[name] = AccessAPI
		}
	}

	return &Router{
		cfg:           config.DefaultConfig(),
		providers:     providers,
		providerCache: cache,
		accessTypes:   access,
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(defaultCircuitFailureThreshold, defaultCircuitCooldown),
		mode:          mode,
		modeSet:       true,
	}
}

// --- Free mode tests ---

func TestModeRouting_FreeModeOnlyUsesNonAPIProviders(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true, failChat: true},
		"claude": {name: "claude", available: true, failChat: false},
	})

	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err == nil {
		t.Fatal("Chat() should fail: free mode must not fall back to API provider claude")
	}
}

func TestModeRouting_FreeModeBlocksSubscriptionProviders(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true, failChat: true},
		"claude": {name: "claude", available: true, failChat: false},
	})
	r.accessTypes["claude"] = AccessSubscription

	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err == nil {
		t.Fatal("Chat() should fail: strict free mode must not use subscription provider claude")
	}
}

func TestModeRouting_FreeModeAllowsLocalProviders(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true, failChat: true},
		"ollama": {name: "ollama", available: true, failChat: false},
		"claude": {name: "claude", available: true, failChat: false},
	})
	r.accessTypes["ollama"] = AccessLocal

	content, _, route, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v, want nil (ollama should handle it)", err)
	}
	if route.Actual != "ollama" {
		t.Errorf("route.Actual = %q, want %q", route.Actual, "ollama")
	}
	if content != "ok" {
		t.Errorf("content = %q, want %q", content, "ok")
	}
}

// --- Economy mode tests ---

func TestModeRouting_EconomyModePrefersFreeBeforeAPI(t *testing.T) {
	r := makeRouter(t, ModeEconomy, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true},
		"claude": {name: "claude", available: true},
	})

	_, _, route, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	// Economy/TaskCode strategy: ["gemini", "claude", ...] with gemini=Free sorting first.
	if route.Actual != "gemini" {
		t.Errorf("route.Actual = %q, want %q (free provider preferred in economy)", route.Actual, "gemini")
	}
}

func TestModeRouting_EconomyFallsBackToAPIWhenFreeDown(t *testing.T) {
	r := makeRouter(t, ModeEconomy, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true, failChat: true},
		"claude": {name: "claude", available: true},
	})

	_, _, route, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if route.Actual != "claude" {
		t.Errorf("route.Actual = %q, want %q (fallback to API)", route.Actual, "claude")
	}
	if !route.IsFallback {
		t.Error("IsFallback = false, want true")
	}
}

// --- Balanced mode tests ---

func TestModeRouting_BalancedModeStaticPrimaries(t *testing.T) {
	r := makeRouter(t, ModeBalanced, map[string]*stubProvider{
		"claude": {name: "claude", available: true},
		"gemini": {name: "gemini", available: true},
		"codex":  {name: "codex", available: true},
	})
	// Set codex to subscription so it's available.
	r.accessTypes["codex"] = AccessSubscription

	// Balanced static primaries from buildStrategyTable.
	if got := r.BuildProviderFor(PhaseCode); got != "claude" {
		t.Errorf("BuildProviderFor(PhaseCode) = %q, want %q", got, "claude")
	}
	if got := r.BuildProviderFor(PhaseReview); got != "gemini" {
		t.Errorf("BuildProviderFor(PhaseReview) = %q, want %q", got, "gemini")
	}
	if got := r.BuildProviderFor(PhaseFix); got != "codex" {
		t.Errorf("BuildProviderFor(PhaseFix) = %q, want %q", got, "codex")
	}
}

// --- Power mode tests ---

func TestModeRouting_PowerModeTierIsPremium(t *testing.T) {
	// Verify the strategy table entries for Power mode use TierPremium.
	for task, entry := range strategyTable[ModePower] {
		if entry.Tier != TierPremium {
			t.Errorf("strategyTable[ModePower][%d].Tier = %v, want TierPremium", task, entry.Tier)
		}
	}
	// Verify models resolve to premium IDs.
	wantClaude := modelTable["claude"][TierPremium]
	if wantClaude != "claude-opus-4-20250514" {
		t.Errorf("modelTable[claude][TierPremium] = %q, want %q", wantClaude, "claude-opus-4-20250514")
	}
	wantGemini := modelTable["gemini"][TierPremium]
	if wantGemini != "gemini-2.5-pro" {
		t.Errorf("modelTable[gemini][TierPremium] = %q, want %q", wantGemini, "gemini-2.5-pro")
	}
}

// --- Circuit breaker tests ---

func TestModeRouting_CircuitBreakerSkipsOpenProvider(t *testing.T) {
	r := makeRouter(t, ModeBalanced, map[string]*stubProvider{
		"claude": {name: "claude", available: true},
		"codex":  {name: "codex", available: true},
		"gemini": {name: "gemini", available: true},
	})
	r.accessTypes["codex"] = AccessSubscription

	// Trip the circuit breaker for claude (3 consecutive failures).
	for i := 0; i < defaultCircuitFailureThreshold; i++ {
		r.breaker.RecordFailure("claude")
	}

	// Balanced/TaskCode: primary is "claude" → should skip to next candidate.
	_, _, route, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if route.Actual == "claude" {
		t.Error("route.Actual = claude, want a fallback (circuit should be open)")
	}
	if !route.IsFallback {
		t.Error("IsFallback = false, want true")
	}
}

func TestModeRouting_AllCircuitsOpenReturnsError(t *testing.T) {
	r := makeRouter(t, ModeBalanced, map[string]*stubProvider{
		"claude": {name: "claude", available: true, failChat: true},
		"gemini": {name: "gemini", available: true, failChat: true},
	})

	// Open circuits for all providers.
	for _, name := range []string{"claude", "gemini"} {
		for i := 0; i < defaultCircuitFailureThreshold; i++ {
			r.breaker.RecordFailure(name)
		}
	}

	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err == nil {
		t.Fatal("Chat() should fail when all circuits are open")
	}
}

func TestModeRouting_FreeModeCircuitOpenBlocksAPIFallback(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true, failChat: true},
		"claude": {name: "claude", available: true},
	})

	// Open gemini circuit.
	for i := 0; i < defaultCircuitFailureThreshold; i++ {
		r.breaker.RecordFailure("gemini")
	}

	// Free mode: gemini circuit open, claude is API → should error, not use claude.
	_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err == nil {
		t.Fatal("Chat() should fail: free mode must not fall back to API even with circuit open primary")
	}
}

// --- Default mode test ---

func TestModeRouting_DefaultsToBalancedWhenUnset(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"claude": {name: "claude", available: true},
	})
	r.modeSet = false // Override: mode not set

	if got := r.effectiveMode(); got != ModeBalanced {
		t.Fatalf("effectiveMode() = %v, want ModeBalanced", got)
	}
	if got := r.BuildProviderFor(PhaseCode); got != "claude" {
		t.Fatalf("BuildProviderFor(PhaseCode) = %q, want %q (ModeBalanced primary)", got, "claude")
	}
}

// --- Cross-model exclusion test ---

func TestModeRouting_CrossModelExclusionInRouteProvider(t *testing.T) {
	r := makeRouter(t, ModeBalanced, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true},
		"codex":  {name: "codex", available: true},
		"claude": {name: "claude", available: true},
	})
	r.accessTypes["codex"] = AccessSubscription

	// RouteProvider("gemini", PhaseReview, "gemini") → gemini is excluded → should use fallback.
	result, err := r.RouteProvider("gemini", PhaseReview, "gemini")
	if err != nil {
		t.Fatalf("RouteProvider error = %v", err)
	}
	if result.Actual == "gemini" {
		t.Error("RouteProvider should not return the excluded provider")
	}
	if !result.IsFallback {
		t.Error("IsFallback = false, want true (excluded provider forces fallback)")
	}
}

func TestModeRouting_BuildProviderForAdaptiveSkipsUnavailable(t *testing.T) {
	r := makeRouter(t, ModeBalanced, map[string]*stubProvider{
		"claude": {name: "claude", available: false}, // static primary for PhaseCode
		"codex":  {name: "codex", available: true},
		"gemini": {name: "gemini", available: true},
	})
	r.accessTypes["claude"] = AccessSubscription
	r.accessTypes["codex"] = AccessSubscription
	r.accessTypes["gemini"] = AccessSubscription

	for i := 0; i < 40; i++ {
		if got := r.BuildProviderForAdaptive(PhaseCode); got == "claude" {
			t.Fatalf("BuildProviderForAdaptive selected unavailable provider %q", got)
		}
	}
}

func TestModeRouting_RouteProviderFreeModeBlocksAPIRequested(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true},
		"claude": {name: "claude", available: true},
	})
	r.accessTypes["claude"] = AccessAPI

	result, err := r.RouteProvider("claude", PhaseCode)
	if err != nil {
		t.Fatalf("RouteProvider error = %v", err)
	}
	if result.Actual == "claude" {
		t.Fatalf("RouteProvider selected API provider in free mode: %q", result.Actual)
	}
}

func TestModeRouting_RouteProviderFreeModeBlocksSubscriptionRequested(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true},
		"claude": {name: "claude", available: true},
	})
	r.accessTypes["claude"] = AccessSubscription

	result, err := r.RouteProvider("claude", PhaseCode)
	if err != nil {
		t.Fatalf("RouteProvider error = %v", err)
	}
	if result.Actual == "claude" {
		t.Fatalf("RouteProvider selected subscription provider in strict free mode: %q", result.Actual)
	}
}

func TestModeRouting_AvailableFreeModeOnlyFreeAndLocal(t *testing.T) {
	r := makeRouter(t, ModeFree, map[string]*stubProvider{
		"gemini": {name: "gemini", available: true},
		"ollama": {name: "ollama", available: true},
		"claude": {name: "claude", available: true},
	})
	r.accessTypes["gemini"] = AccessFree
	r.accessTypes["ollama"] = AccessLocal
	r.accessTypes["claude"] = AccessSubscription

	avail := r.Available()
	for _, name := range avail {
		if name == "claude" {
			t.Fatalf("Available() included subscription provider in strict free mode: %v", avail)
		}
	}
}

func TestModeRouting_ChatWithAdaptiveFallbackUsesQualitySignal(t *testing.T) {
	const runs = 40
	geminiWins := 0

	for i := 0; i < runs; i++ {
		r := makeRouter(t, ModeBalanced, map[string]*stubProvider{
			"codex":  {name: "codex", available: true, failChat: true}, // requested provider fails
			"claude": {name: "claude", available: true},
			"gemini": {name: "gemini", available: true},
		})
		r.accessTypes["codex"] = AccessSubscription
		r.accessTypes["claude"] = AccessSubscription
		r.accessTypes["gemini"] = AccessSubscription

		// PhaseFix static fallback order prefers claude before gemini.
		// Strong quality data should make adaptive fallback pick gemini most of the time.
		for j := 0; j < 30; j++ {
			r.usage.RecordQualityOutcome(PhaseFix, "gemini", true)
		}
		for j := 0; j < 20; j++ {
			r.usage.RecordQualityOutcome(PhaseFix, "claude", false)
		}

		_, _, route, err := r.ChatWith(context.Background(), "codex", PhaseFix,
			[]Message{{Role: "user", Content: "fix failing go test"}},
			"You are a fixer.")
		if err != nil {
			t.Fatalf("ChatWith error = %v", err)
		}
		if route.Actual == "gemini" {
			geminiWins++
		}
	}

	if geminiWins < runs*7/10 {
		t.Fatalf("adaptive fallback chose gemini %d/%d times; want >= %d",
			geminiWins, runs, runs*7/10)
	}
}

// --- Thompson Sampling convergence test ---

func TestModeRouting_ThompsonSamplingConvergesWithQualityData(t *testing.T) {
	r := makeRouter(t, ModeBalanced, map[string]*stubProvider{
		"claude": {name: "claude", available: true},
		"codex":  {name: "codex", available: true},
		"gemini": {name: "gemini", available: true},
	})
	r.accessTypes["codex"] = AccessSubscription

	// Record strong quality signal: codex succeeds 20 times, claude fails 10 times.
	for i := 0; i < 20; i++ {
		r.usage.RecordQualityOutcome(PhaseCode, "codex", true)
	}
	for i := 0; i < 10; i++ {
		r.usage.RecordQualityOutcome(PhaseCode, "claude", false)
	}

	codexWins := 0
	const runs = 30
	for i := 0; i < runs; i++ {
		if r.BuildProviderForAdaptive(PhaseCode) == "codex" {
			codexWins++
		}
	}

	threshold := runs * 8 / 10 // 80%
	if codexWins < threshold {
		t.Errorf("codex won %d/%d times, want >= %d (Thompson Sampling should converge on high-quality provider)",
			codexWins, runs, threshold)
	}
}

// --- RegisterProvider test ---

func TestModeRouting_RegisterProviderAndRoute(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DefaultModel = "custom"
	cfg.CodingModel = "custom"

	r := &Router{
		cfg:           cfg,
		providers:     make(map[string]Provider),
		providerCache: make(map[providerKey]Provider),
		accessTypes:   make(map[string]AccessType),
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(defaultCircuitFailureThreshold, defaultCircuitCooldown),
		modeSet:       false, // legacy routing path
	}

	custom := &stubProvider{name: "custom", available: true}
	if err := r.RegisterProvider("custom", custom, AccessAPI); err != nil {
		t.Fatalf("RegisterProvider error = %v", err)
	}

	// Legacy routing: CodingModel="custom" → should find the registered provider.
	result, err := r.routeLegacy(TaskCode)
	if err != nil {
		t.Fatalf("routeLegacy error = %v", err)
	}
	if result.Actual != "custom" {
		t.Errorf("routeLegacy.Actual = %q, want %q", result.Actual, "custom")
	}

	// Verify via Chat.
	content, _, route, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "test"}}, "")
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if route.Actual != "custom" {
		t.Errorf("Chat route.Actual = %q, want %q", route.Actual, "custom")
	}
	if content != "ok" {
		t.Errorf("content = %q, want %q", content, "ok")
	}
}

// --- Helper: freeze breaker clock for deterministic circuit tests ---

func freezeBreakerClock(r *Router, now time.Time) {
	r.breaker.now = func() time.Time { return now }
}
