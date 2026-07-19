package router

// instance_isolation_test.go: regression tests for per-instance router state.
//
// P0 – strategy tables are per-Router: overrides and hot-reloads on one
//      instance never leak into another instance or the package defaults
// P0 – NewRouterFromConfig surfaces invalid overrides instead of logging them
// P0 – user overrides deep-merge over defaults (absent fields keep defaults)
// P1 – half-open circuit admits exactly one concurrent probe
// P1 – concurrent stats saves are serialized and debounced

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeRoutingJSON(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "routing.json"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestRouters_IndependentOverrides verifies that two Routers built from
// different ConfigDirs each see only their own overrides, and that a third
// Router without overrides keeps the embedded defaults.
func TestRouters_IndependentOverrides(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeRoutingJSON(t, dirA, `{"strategies":{"balanced":{"code":{"tier":"mid","providers":["gemini"]}}}}`)
	writeRoutingJSON(t, dirB, `{"strategies":{"balanced":{"code":{"tier":"mid","providers":["codex"]}}}}`)

	rA := mustNewRouter(RouterConfig{ConfigDir: dirA, UsageMode: "balanced"})
	rB := mustNewRouter(RouterConfig{ConfigDir: dirB, UsageMode: "balanced"})
	rC := mustNewRouter(RouterConfig{UsageMode: "balanced"})

	entryA, _ := rA.routingTables().strategyFor(ModeBalanced, TaskCode)
	entryB, _ := rB.routingTables().strategyFor(ModeBalanced, TaskCode)
	entryC, _ := rC.routingTables().strategyFor(ModeBalanced, TaskCode)

	if len(entryA.Providers) != 1 || entryA.Providers[0] != "gemini" {
		t.Errorf("router A providers = %v, want [gemini]", entryA.Providers)
	}
	if len(entryB.Providers) != 1 || entryB.Providers[0] != "codex" {
		t.Errorf("router B providers = %v, want [codex]", entryB.Providers)
	}
	if len(entryC.Providers) == 0 || entryC.Providers[0] != "claude" {
		t.Errorf("override-free router providers = %v, want default order starting with claude", entryC.Providers)
	}

	// The embedded package defaults must be untouched by any instance override.
	base, _ := baseTables.strategyFor(ModeBalanced, TaskCode)
	if len(base.Providers) == 0 || base.Providers[0] != "claude" {
		t.Errorf("package defaults polluted: providers = %v, want default order starting with claude", base.Providers)
	}
}

// TestLoadUserOverrides_DeepMergeKeepsDefaults verifies the README promise:
// only the fields present in routing.json are overridden, down to individual
// (provider, tier) keys.
func TestLoadUserOverrides_DeepMergeKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	// Override only claude's mid model; every other tier and provider keeps
	// its default. The target ID already exists in the default cost table.
	writeRoutingJSON(t, dir, `{"models":{"claude":{"mid":"claude-opus-4-20250514"}}}`)

	r := mustNewRouter(RouterConfig{ConfigDir: dir})
	tables := r.routingTables()

	if got := tables.modelID("claude", TierMid); got != "claude-opus-4-20250514" {
		t.Errorf("claude mid model = %q, want overridden %q", got, "claude-opus-4-20250514")
	}
	if got, want := tables.modelID("claude", TierCheap), baseTables.modelID("claude", TierCheap); got != want {
		t.Errorf("claude cheap model = %q, want default %q (deep merge must keep it)", got, want)
	}
	if got, want := tables.modelID("gemini", TierMid), baseTables.modelID("gemini", TierMid); got != want {
		t.Errorf("gemini mid model = %q, want default %q", got, want)
	}
}

// TestNewRouterFromConfig_InvalidOverridesError verifies that broken overrides
// fail construction instead of being logged to stderr and ignored.
func TestNewRouterFromConfig_InvalidOverridesError(t *testing.T) {
	dir := t.TempDir()

	writeRoutingJSON(t, dir, `{invalid json}`)
	if _, err := NewRouterFromConfig(RouterConfig{ConfigDir: dir}); err == nil {
		t.Fatal("NewRouterFromConfig accepted malformed routing.json, want error")
	}

	// Parses fine but fails semantic validation: unknown provider in a strategy.
	writeRoutingJSON(t, dir, `{"strategies":{"balanced":{"code":{"tier":"mid","providers":["ghost"]}}}}`)
	if _, err := NewRouterFromConfig(RouterConfig{ConfigDir: dir}); err == nil {
		t.Fatal("NewRouterFromConfig accepted semantically invalid routing.json, want error")
	}
}

// TestWatchOverrides_InvalidOverrideKeepsOldTables verifies the hot-reload
// path validates candidates and keeps the previous snapshot on failure.
func TestWatchOverrides_InvalidOverrideKeepsOldTables(t *testing.T) {
	dir := t.TempDir()
	r := mustNewRouter(RouterConfig{UsageMode: "balanced"})

	var mu sync.Mutex
	var traces []TraceEvent
	r.SetTraceSink(TraceSinkFunc(func(e TraceEvent) {
		mu.Lock()
		traces = append(traces, e)
		mu.Unlock()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.WatchOverridesInterval(ctx, dir, 50*time.Millisecond)

	// Parses fine but references a provider without a model table entry.
	writeRoutingJSON(t, dir, `{"strategies":{"balanced":{"code":{"tier":"mid","providers":["ghost"]}}}}`)
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	found := false
	for _, e := range traces {
		if e.Event == "reload_error" {
			found = true
			break
		}
	}
	mu.Unlock()
	if !found {
		t.Fatal("expected reload_error trace event for invalid override")
	}

	entry, ok := r.routingTables().strategyFor(ModeBalanced, TaskCode)
	if !ok || len(entry.Providers) == 0 || entry.Providers[0] != "claude" {
		t.Fatalf("tables changed after invalid override: %+v", entry)
	}
}

// TestCircuitBreaker_HalfOpenAllowsSingleProbe verifies that once the cooldown
// elapses, exactly one concurrent caller is admitted as the half-open trial
// and every other caller is rejected until the probe resolves.
func TestCircuitBreaker_HalfOpenAllowsSingleProbe(t *testing.T) {
	now := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)
	cb := newProviderCircuitBreaker(1, 10*time.Second)
	cb.now = func() time.Time { return now }

	if opened, _ := cb.RecordFailure("claude"); !opened {
		t.Fatal("threshold=1 failure should open the circuit")
	}
	now = now.Add(11 * time.Second)

	var allowed int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := cb.BeforeAttempt("claude"); ok {
				atomic.AddInt32(&allowed, 1)
			}
		}()
	}
	wg.Wait()
	if allowed != 1 {
		t.Fatalf("half-open admitted %d concurrent probes, want exactly 1", allowed)
	}

	// While the probe is outstanding, later callers are still rejected.
	if ok, _ := cb.BeforeAttempt("claude"); ok {
		t.Fatal("BeforeAttempt admitted a second caller while the probe is in flight")
	}

	// Probe failure re-opens the circuit immediately.
	if opened, _ := cb.RecordFailure("claude"); !opened {
		t.Fatal("failure in half-open should re-open the circuit")
	}
	if ok, _ := cb.BeforeAttempt("claude"); ok {
		t.Fatal("BeforeAttempt should block after a failed probe")
	}

	// Next cooldown expiry admits one probe again; success closes the circuit.
	now = now.Add(11 * time.Second)
	if ok, _ := cb.BeforeAttempt("claude"); !ok {
		t.Fatal("cooldown expiry should admit a new probe")
	}
	cb.RecordSuccess("claude")
	if ok, _ := cb.BeforeAttempt("claude"); !ok {
		t.Fatal("BeforeAttempt should allow traffic after a successful probe")
	}
}

// TestSessionUsage_ConcurrentSaveIsSafe hammers Save from multiple goroutines
// while stats are being recorded. Run with -race locally.
func TestSessionUsage_ConcurrentSaveIsSafe(t *testing.T) {
	dir := t.TempDir()
	s := newSessionUsage()

	var wg sync.WaitGroup
	const workers = 8
	const iterations = 50
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				s.Increment("claude")
				s.RecordQualityOutcome(PhaseCode, "claude", j%2 == 0)
				if err := s.Save(dir); err != nil {
					t.Errorf("concurrent Save error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := s.Save(dir); err != nil {
		t.Fatalf("final Save error = %v", err)
	}
	loaded := newSessionUsage()
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if got := loaded.Count("claude"); got != workers*iterations {
		t.Fatalf("loaded claude count = %d, want %d", got, workers*iterations)
	}
}

// TestSessionUsage_SaveDebounced verifies the dirty-flag and minimum-interval
// behavior of the per-request save path, and that explicit Save always flushes.
func TestSessionUsage_SaveDebounced(t *testing.T) {
	dir := t.TempDir()
	statsPath := filepath.Join(dir, statsFile)
	s := newSessionUsage()

	s.Increment("claude")
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save error = %v", err)
	}
	if err := os.Remove(statsPath); err != nil {
		t.Fatal(err)
	}

	// No new data since the last save: the debounced write is skipped.
	if err := s.saveDebounced(dir); err != nil {
		t.Fatalf("saveDebounced error = %v", err)
	}
	if _, err := os.Stat(statsPath); !os.IsNotExist(err) {
		t.Fatal("saveDebounced wrote despite no new data")
	}

	// New data, but the last write was under minStatsSaveInterval ago: skipped.
	s.Increment("claude")
	if err := s.saveDebounced(dir); err != nil {
		t.Fatalf("saveDebounced error = %v", err)
	}
	if _, err := os.Stat(statsPath); !os.IsNotExist(err) {
		t.Fatal("saveDebounced wrote inside the minimum interval")
	}

	// An explicit Save always flushes.
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save error = %v", err)
	}
	if _, err := os.Stat(statsPath); err != nil {
		t.Fatalf("Save did not flush pending stats: %v", err)
	}
}
