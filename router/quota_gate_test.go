package router

import (
	"context"
	"testing"
	"time"
)

func newTestRouterWithQuota(q QuotaController) *Router {
	return NewRouterFromConfig(RouterConfig{Quota: q})
}

func TestQuotaBandForReflectsSnapshot(t *testing.T) {
	src := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(95)}}
	snap := NewQuotaSnapshotter(time.Hour, src)
	snap.Refresh(context.Background())
	r := newTestRouterWithQuota(snap)

	if got := r.quotaBandFor("claude"); got != QuotaBandCritical {
		t.Fatalf("95%% weekly should be critical, got %d", got)
	}
	if got := r.quotaBandFor("unknown"); got != QuotaBandOK {
		t.Fatalf("unknown provider should be OK (neutral), got %d", got)
	}
	// Nil controller → always OK.
	if got := (&Router{}).quotaBandFor("claude"); got != QuotaBandOK {
		t.Fatalf("nil quota should be OK, got %d", got)
	}
}

func TestQuotaBandReordersCandidates(t *testing.T) {
	// Two candidates, equal on every other signal; the exhausted one must sort last.
	cands := []candidate{
		{name: "claude", quotaBand: QuotaBandCritical, order: 0},
		{name: "codex", quotaBand: QuotaBandOK, order: 1},
	}
	sortCandidatesForMode(cands, ModeBalanced)
	if cands[0].name != "codex" {
		t.Fatalf("OK-band provider should sort first, got %s", cands[0].name)
	}
}

func TestQuotaHardSealBlocksExecution(t *testing.T) {
	src := &fakeSource{name: "codex", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(50)}}
	snap := NewQuotaSnapshotter(time.Hour, src)
	snap.Refresh(context.Background())
	r := newTestRouterWithQuota(snap)

	// Not sealed → allowed (no circuit breaker tripping in a fresh router).
	if allow, _ := r.beforeProviderAttempt("codex"); !allow {
		t.Fatalf("healthy provider should be allowed")
	}

	// Seal it (simulating a 429 with a known reset) → hard-blocked.
	snap.MarkExhausted("codex", time.Now().Add(time.Hour))
	if allow, remaining := r.beforeProviderAttempt("codex"); allow || remaining <= 0 {
		t.Fatalf("sealed provider must be blocked with positive remaining, allow=%v remaining=%v", allow, remaining)
	}
}

func TestNoteQuotaErrorSealsOnRateLimit(t *testing.T) {
	src := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(88)}}
	snap := NewQuotaSnapshotter(time.Hour, src)
	snap.Refresh(context.Background())
	r := newTestRouterWithQuota(snap)

	// A non-rate-limit error must not seal.
	r.noteQuotaError("claude", newProviderError("claude", "CLI", ErrorKindNetwork, true, 0, "reset", nil))
	if sealed, _ := r.quotaHardBlocked("claude"); sealed {
		t.Fatalf("network error must not seal")
	}

	// A rate-limit error must seal until reset.
	r.noteQuotaError("claude", newProviderError("claude", "CLI", ErrorKindRateLimit, false, 0, "429", nil))
	if sealed, _ := r.quotaHardBlocked("claude"); !sealed {
		t.Fatalf("rate-limit error should seal the provider")
	}
}

func TestNilQuotaIsNoOp(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{}) // no quota controller
	if allow, _ := r.beforeProviderAttempt("claude"); !allow {
		t.Fatalf("with nil quota, provider should be allowed")
	}
	r.noteQuotaError("claude", newProviderError("claude", "CLI", ErrorKindRateLimit, false, 0, "429", nil))
	// Should not panic and should remain unblocked.
	if sealed, _ := r.quotaHardBlocked("claude"); sealed {
		t.Fatalf("nil quota must never seal")
	}
}
