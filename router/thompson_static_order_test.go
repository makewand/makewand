package router

import (
	"context"
	"testing"
)

func threeSubscriptionStubs(t *testing.T, mode UsageMode) *Router {
	t.Helper()
	r := makeRouter(t, mode, map[string]*stubProvider{
		"claude": {name: "claude", available: true},
		"codex":  {name: "codex", available: true},
		"gemini": {name: "gemini", available: true},
	})
	for _, name := range []string{"claude", "codex", "gemini"} {
		r.accessTypes[name] = AccessSubscription
	}
	return r
}

// TestThompsonKeepsStaticOrderWithoutQualitySignal reproduces go-router#3: plain
// Chat/HTTP traffic only increments request counts (never RecordQualityOutcome),
// yet after the first request the sort left the cold-start guard and ranked by
// pure Beta-prior noise, picking the static primary only ~52% of the time.
func TestThompsonKeepsStaticOrderWithoutQualitySignal(t *testing.T) {
	r := threeSubscriptionStubs(t, ModeBalanced)
	entry, ok := r.routingTables().strategyFor(ModeBalanced, TaskCode)
	if !ok || len(entry.Providers) == 0 {
		t.Fatal("no balanced/code strategy")
	}
	primary := entry.Providers[0]

	const runs = 400
	counts := map[string]int{}
	for i := 0; i < runs; i++ {
		_, _, res, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "hi"}}, "")
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		counts[res.Actual]++
	}
	if counts[primary] != runs {
		t.Fatalf("static primary %q chosen %d/%d times without any quality signal (distribution %v); want the static order every time",
			primary, counts[primary], runs, counts)
	}
}

// TestThompsonInsufficientQualitySamplesKeepStaticOrder: fewer than
// minQualitySamplesForThompson outcomes is not evidence either.
func TestThompsonInsufficientQualitySamplesKeepStaticOrder(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := []candidate{
			{name: "first", access: AccessSubscription, order: 0, requests: 40, useCount: 40, qualitySamples: minQualitySamplesForThompson - 1, thompsonScore: 0.05},
			{name: "second", access: AccessSubscription, order: 1, requests: 3, useCount: 3, qualitySamples: 0, thompsonScore: 0.99},
		}
		sortCandidatesForMode(c, ModeBalanced)
		if c[0].name != "first" {
			t.Fatalf("iteration %d: %q sorted first; below-threshold quality samples must keep static order", i, c[0].name)
		}
	}
}

// TestThompsonColdPairKeepsStaticOrderNextToWarmProvider: when only one provider
// has real quality evidence, the providers without evidence must still keep
// their static order relative to each other (only the warm one may move).
func TestThompsonColdPairKeepsStaticOrderNextToWarmProvider(t *testing.T) {
	r := threeSubscriptionStubs(t, ModeBalanced)
	entry, _ := r.routingTables().strategyFor(ModeBalanced, TaskCode)
	if len(entry.Providers) < 3 {
		t.Skip("strategy has fewer than three providers")
	}
	coldA, coldB, warm := entry.Providers[0], entry.Providers[1], entry.Providers[2]
	for i := 0; i < 20; i++ {
		r.usage.RecordQualityOutcome(PhaseCode, warm, true)
	}
	// Ordinary traffic: request counts are not quality evidence.
	for i := 0; i < 5; i++ {
		r.usage.Increment(coldA)
		r.usage.Increment(coldB)
	}
	for i := 0; i < 300; i++ {
		cands := r.modeCandidates(entry, nil, PhaseCode)
		pos := map[string]int{}
		for idx, c := range cands {
			pos[c.name] = idx
		}
		if pos[coldA] > pos[coldB] {
			t.Fatalf("iteration %d: cold %q sorted after cold %q (%v); providers without quality evidence must keep static order",
				i, coldA, coldB, cands)
		}
	}
}
