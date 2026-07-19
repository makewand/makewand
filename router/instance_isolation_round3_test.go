package router

// instance_isolation_round3_test.go: regression tests for the round-3 fixes.
//
// #1 strict decode rejects trailing tokens ("{} }") and a top-level null
// #2 deleting routing.json on reload reverts to the immutable defaults
// #4 a provider instance shared by two Routers prices each Router's calls from
//    that Router's own overrides (no last-writer-wins pointer overwrite)
// #5 a struct-literal / zero-value Router gets its own copy of the defaults and
//    is never affected by the package-level LoadUserOverrides

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Finding #1: strict trailing-data / null rejection --------------------

func TestDecodeRawDefaults_TrailingAndNull(t *testing.T) {
	reject := []struct {
		name string
		in   string
	}{
		{"stray closing brace", `{} }`},
		{"stray closing bracket", `{} ]`},
		{"top-level null", `null`},
		{"top-level null with whitespace", "  null\n"},
		{"two objects", `{} {}`},
		{"trailing garbage", `{"models":{}} garbage`},
	}
	for _, tc := range reject {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			if _, err := decodeRawDefaults([]byte(tc.in)); err == nil {
				t.Fatalf("decodeRawDefaults(%q) = nil error, want rejection", tc.in)
			}
		})
	}

	accept := []struct {
		name string
		in   string
	}{
		{"single object", `{"models":{"claude":{"mid":"claude-sonnet-4-20250514"}}}`},
		{"empty object", `{}`},
		{"trailing whitespace", `{"models":{}}   `},
		{"trailing newline", "{\"models\":{}}\n"},
		{"leading whitespace", "  {\"models\":{}}"},
	}
	for _, tc := range accept {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if _, err := decodeRawDefaults([]byte(tc.in)); err != nil {
				t.Fatalf("decodeRawDefaults(%q) = %v, want success", tc.in, err)
			}
		})
	}
}

// --- Finding #2: deleting routing.json reverts to defaults ----------------

func TestWatchOverrides_RemovingFileRevertsToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.json")

	r := mustNewRouter(RouterConfig{UsageMode: "balanced"})
	def := baseTables.modelID("claude", TierMid)
	if def == "claude-opus-4-20250514" {
		t.Fatal("test precondition: default claude mid must differ from the override")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.WatchOverridesInterval(ctx, dir, 25*time.Millisecond)

	// Apply an override via the file and wait for the watcher to pick it up.
	writeRoutingJSON(t, dir, `{"models":{"claude":{"mid":"claude-opus-4-20250514"}}}`)
	waitFor(t, time.Second, func() bool {
		return r.routingTables().modelID("claude", TierMid) == "claude-opus-4-20250514"
	}, "override to apply")

	// Remove the whole file: the next reload must revert to the immutable defaults.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return r.routingTables().modelID("claude", TierMid) == def
	}, "defaults to be restored after file removal")
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- Finding #4: shared provider prices per-Router ------------------------

// costAwareStub is a fake API provider that embeds instanceCostTable and prices
// exactly the way the real Claude/Gemini/OpenAI providers do — from the per-call
// snapshot the owning Router injects into the request context.
type costAwareStub struct {
	instanceCostTable
	available bool
}

func (p *costAwareStub) Name() string      { return "claude" }
func (p *costAwareStub) IsAvailable() bool { return p.available }

func (p *costAwareStub) Chat(ctx context.Context, _ []Message, _ string, _ int) (string, Usage, error) {
	model, _ := ModelFromContext(ctx)
	const in, out = 1_000_000, 1_000_000
	return "ok", Usage{
		Provider:     "claude",
		Model:        model,
		InputTokens:  in,
		OutputTokens: out,
		Cost:         p.priceForCtx(ctx, model, in, out),
	}, nil
}

func (p *costAwareStub) ChatStream(_ context.Context, _ []Message, _ string, _ int) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func TestSharedProvider_PricesPerRouter(t *testing.T) {
	const model = "claude-sonnet-4-20250514" // balanced/code → claude mid

	dirA := t.TempDir()
	dirB := t.TempDir()
	// Router A prices sonnet at $9/$36, Router B at $1/$2 per MTok.
	writeRoutingJSON(t, dirA, `{"costs":{"`+model+`":{"input":9,"output":36}}}`)
	writeRoutingJSON(t, dirB, `{"costs":{"`+model+`":{"input":1,"output":2}}}`)

	// One provider instance shared by both Routers. Both call attachCostTable on
	// it; without the per-call ctx snapshot the second Router's attach would win
	// and both Routers would price identically.
	shared := &costAwareStub{available: true}

	rA := mustNewRouter(RouterConfig{
		ConfigDir: dirA,
		UsageMode: "balanced",
		Providers: map[string]ProviderEntry{"claude": {Provider: shared, Access: AccessAPI}},
	})
	rB := mustNewRouter(RouterConfig{
		ConfigDir: dirB,
		UsageMode: "balanced",
		Providers: map[string]ProviderEntry{"claude": {Provider: shared, Access: AccessAPI}},
	})

	msgs := []Message{{Role: "user", Content: "hi"}}

	_, usageA, _, err := rA.Chat(context.Background(), TaskCode, msgs, "")
	if err != nil {
		t.Fatalf("rA.Chat: %v", err)
	}
	_, usageB, _, err := rB.Chat(context.Background(), TaskCode, msgs, "")
	if err != nil {
		t.Fatalf("rB.Chat: %v", err)
	}

	if usageA.Model != model || usageB.Model != model {
		t.Fatalf("routed model = %q/%q, want %q (test assumes balanced/code→claude mid)", usageA.Model, usageB.Model, model)
	}
	if got, want := usageA.Cost, 9.0+36.0; got != want {
		t.Errorf("router A cost = %v, want %v", got, want)
	}
	if got, want := usageB.Cost, 1.0+2.0; got != want {
		t.Errorf("router B cost = %v, want %v (shared provider mispriced by the other router)", got, want)
	}
}

// --- Finding #5: zero-value Router gets its own tables --------------------

// TestZeroValueRouter_HasPrivateTables verifies a struct-literal Router lazily
// adopts its own copy of the defaults, never the shared mutable defaultTables
// (nor the immutable baseTables its own overrides could corrupt).
func TestZeroValueRouter_HasPrivateTables(t *testing.T) {
	r := &Router{}
	tbls := r.routingTables()
	if tbls == nil {
		t.Fatal("zero-value router returned nil tables")
	}
	if tbls == defaultTables {
		t.Fatal("zero-value router shares the mutable package defaultTables")
	}
	if tbls == baseTables {
		t.Fatal("zero-value router shares the immutable baseTables (its overrides would corrupt defaults)")
	}
	if again := r.routingTables(); again != tbls {
		t.Fatal("routingTables adopted a different instance on the second call")
	}
	if got, want := tbls.modelID("claude", TierMid), baseTables.modelID("claude", TierMid); got != want {
		t.Fatalf("zero-value router claude mid = %q, want default %q", got, want)
	}
}

// TestPackageLoadUserOverrides_DoesNotAffectZeroValueRouter verifies that
// mutating the package-level defaultTables (via the deprecated package-level
// LoadUserOverrides) never leaks into a struct-literal Router.
func TestPackageLoadUserOverrides_DoesNotAffectZeroValueRouter(t *testing.T) {
	r := &Router{}
	before := r.routingTables().modelID("claude", TierMid)

	dir := t.TempDir()
	writeRoutingJSON(t, dir, `{"models":{"claude":{"mid":"claude-opus-4-20250514"}}}`)
	// Restore the package defaults so this global mutation can't pollute other tests.
	t.Cleanup(func() { defaultTables.resetToDefaults() })

	if err := LoadUserOverrides(dir); err != nil {
		t.Fatalf("LoadUserOverrides: %v", err)
	}
	// Sanity: the package-level tables actually changed.
	if defaultTables.modelID("claude", TierMid) != "claude-opus-4-20250514" {
		t.Fatal("precondition: package LoadUserOverrides did not mutate defaultTables")
	}
	// The zero-value router must be unaffected.
	if got := r.routingTables().modelID("claude", TierMid); got != before {
		t.Fatalf("zero-value router leaked package override: got %q, want %q", got, before)
	}
}
