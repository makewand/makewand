package router

// instance_isolation_round2_test.go: regression tests for the round-2
// instance-isolation fixes.
//
// #1 provider factories are strictly per-instance (no package-level bleed)
// #2 a Router's custom price override prices non-streaming calls
// #4 overrides merge at field granularity (partial entry keeps other fields)
// #5 strict validation rejects unknown fields / empty providers / bad enums
// #6 removing an override on reload reverts to the default

import "testing"

// TestRouters_IndependentProviderFactories verifies that a factory registered
// on one Router is invisible to every other Router (finding #1). Before the
// fix, a package-level registry meant one registration influenced all Routers.
func TestRouters_IndependentProviderFactories(t *testing.T) {
	const name = "factory-isolation"

	rA := mustNewRouter(RouterConfig{})
	rB := mustNewRouter(RouterConfig{})

	if err := rA.RegisterProviderFactory(name, func(string) (Provider, error) {
		return &stubProvider{name: "A", available: true}, nil
	}); err != nil {
		t.Fatalf("rA RegisterProviderFactory: %v", err)
	}
	if err := rB.RegisterProviderFactory(name, func(string) (Provider, error) {
		return &stubProvider{name: "B", available: true}, nil
	}); err != nil {
		t.Fatalf("rB RegisterProviderFactory: %v", err)
	}

	pA, err := rA.resolveProvider(name, "m")
	if err != nil {
		t.Fatalf("rA resolveProvider: %v", err)
	}
	pB, err := rB.resolveProvider(name, "m")
	if err != nil {
		t.Fatalf("rB resolveProvider: %v", err)
	}
	if pA.Name() != "A" || pB.Name() != "B" {
		t.Fatalf("factory bleed: rA resolved %q, rB resolved %q", pA.Name(), pB.Name())
	}

	// A Router that registered nothing must not see either factory.
	rC := mustNewRouter(RouterConfig{})
	if _, err := rC.resolveProvider(name, "m"); err == nil {
		t.Fatal("router C resolved a factory it never registered (global fallback leaked)")
	}
}

// TestRouter_NonStreamingCostUsesInstanceOverride verifies that a Router with a
// custom price override prices a non-streaming response from its own snapshot,
// while an override-free Router keeps the package defaults (finding #2).
func TestRouter_NonStreamingCostUsesInstanceOverride(t *testing.T) {
	const model = "claude-sonnet-4-20250514"
	const oneM = 1_000_000

	dir := t.TempDir()
	// Override sonnet pricing to $9/$36 per MTok (defaults are $3/$15).
	writeRoutingJSON(t, dir, `{"costs":{"`+model+`":{"input":9,"output":36}}}`)

	overridden := NewClaude("key", model)
	mustNewRouter(RouterConfig{
		ConfigDir: dir,
		Providers: map[string]ProviderEntry{"claude": {Provider: overridden, Access: AccessAPI}},
	})

	// priceFor is exactly what Claude.Chat writes into Usage.Cost.
	if got, want := overridden.priceFor(model, oneM, oneM), 9.0+36.0; got != want {
		t.Fatalf("overridden non-streaming cost = %v, want %v", got, want)
	}

	// An override-free Router prices the same model from the defaults.
	def := NewClaude("key", model)
	mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{"claude": {Provider: def, Access: AccessAPI}},
	})
	if got, want := def.priceFor(model, oneM, oneM), 3.0+15.0; got != want {
		t.Fatalf("default non-streaming cost = %v, want %v (override leaked)", got, want)
	}
}

// TestApplyOverrides_PartialCostKeepsOtherField verifies field-level cost merge:
// overriding only "input" keeps the default "output", and an explicit 0 is
// honored rather than treated as absent (finding #4).
func TestApplyOverrides_PartialCostKeepsOtherField(t *testing.T) {
	dir := t.TempDir()
	// sonnet default: input 3, output 15. gemini-pro default: input 1.25, output 10.
	writeRoutingJSON(t, dir, `{"costs":{
		"claude-sonnet-4-20250514":{"input":7},
		"gemini-2.5-pro":{"input":0}
	}}`)
	r := mustNewRouter(RouterConfig{ConfigDir: dir})
	tables := r.routingTables()

	sonnet, ok := tables.costFor("claude-sonnet-4-20250514")
	if !ok {
		t.Fatal("sonnet cost entry missing")
	}
	if sonnet.Input != 7 {
		t.Errorf("sonnet input = %v, want overridden 7", sonnet.Input)
	}
	if sonnet.Output != 15 {
		t.Errorf("sonnet output = %v, want default 15 (partial merge must keep it)", sonnet.Output)
	}

	pro, ok := tables.costFor("gemini-2.5-pro")
	if !ok {
		t.Fatal("gemini-pro cost entry missing")
	}
	if pro.Input != 0 {
		t.Errorf("gemini-pro input = %v, want explicit 0", pro.Input)
	}
	if pro.Output != 10 {
		t.Errorf("gemini-pro output = %v, want default 10 (explicit 0 must not zero it)", pro.Output)
	}
}

// TestApplyOverrides_PartialStrategyKeepsOtherField verifies that overriding
// only a strategy's providers keeps the default tier and vice versa (finding #4).
func TestApplyOverrides_PartialStrategyKeepsOtherField(t *testing.T) {
	// Providers-only override → tier stays default (mid).
	dir := t.TempDir()
	writeRoutingJSON(t, dir, `{"strategies":{"balanced":{"code":{"providers":["gemini"]}}}}`)
	r := mustNewRouter(RouterConfig{ConfigDir: dir})
	entry, _ := r.routingTables().strategyFor(ModeBalanced, TaskCode)
	if entry.Tier != TierMid {
		t.Errorf("tier = %v, want default TierMid for a providers-only override", entry.Tier)
	}
	if len(entry.Providers) != 1 || entry.Providers[0] != "gemini" {
		t.Errorf("providers = %v, want [gemini]", entry.Providers)
	}

	// Tier-only override → providers stay the default order.
	dir2 := t.TempDir()
	writeRoutingJSON(t, dir2, `{"strategies":{"balanced":{"code":{"tier":"premium"}}}}`)
	r2 := mustNewRouter(RouterConfig{ConfigDir: dir2})
	entry2, _ := r2.routingTables().strategyFor(ModeBalanced, TaskCode)
	if entry2.Tier != TierPremium {
		t.Errorf("tier = %v, want overridden TierPremium", entry2.Tier)
	}
	base, _ := baseTables.strategyFor(ModeBalanced, TaskCode)
	if len(entry2.Providers) != len(base.Providers) || entry2.Providers[0] != base.Providers[0] {
		t.Errorf("providers = %v, want default %v for a tier-only override", entry2.Providers, base.Providers)
	}
}

// TestApplyOverrides_StrictValidationRejects verifies strict decoding and
// validation reject malformed overrides instead of silently defaulting
// (finding #5).
func TestApplyOverrides_StrictValidationRejects(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"unknown top-level field", `{"modelz":{}}`},
		{"unknown nested field", `{"strategies":{"balanced":{"code":{"teir":"mid"}}}}`},
		{"unknown cost field", `{"costs":{"claude-sonnet-4-20250514":{"in":3}}}`},
		{"empty providers list", `{"strategies":{"balanced":{"code":{"providers":[]}}}}`},
		{"bad tier", `{"strategies":{"balanced":{"code":{"tier":"ultra"}}}}`},
		{"bad mode", `{"strategies":{"turbo":{"code":{"providers":["claude"]}}}}`},
		{"bad task", `{"strategies":{"balanced":{"refactor":{"providers":["claude"]}}}}`},
		{"bad phase", `{"build_strategies":{"balanced":{"deploy":{"primary":"claude"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRoutingJSON(t, dir, tc.content)
			if _, err := NewRouterFromConfig(RouterConfig{ConfigDir: dir}); err == nil {
				t.Fatalf("accepted invalid override %q, want error", tc.content)
			}
		})
	}
}

// TestApplyOverrides_ReloadRevertsRemovedOverride verifies that recomputing from
// the immutable defaults makes reload idempotent: removing an override reverts
// to the default rather than leaving the previous override in place (finding #6).
func TestApplyOverrides_ReloadRevertsRemovedOverride(t *testing.T) {
	r := mustNewRouter(RouterConfig{UsageMode: "balanced"})
	tables := r.routingTables()

	def := baseTables.modelID("claude", TierMid)
	if def == "claude-opus-4-20250514" {
		t.Fatal("test precondition: default claude mid must differ from the override")
	}

	if err := tables.applyOverrides([]byte(`{"models":{"claude":{"mid":"claude-opus-4-20250514"}}}`)); err != nil {
		t.Fatalf("applyOverrides (set): %v", err)
	}
	if got := tables.modelID("claude", TierMid); got != "claude-opus-4-20250514" {
		t.Fatalf("claude mid after override = %q, want opus", got)
	}

	// Reload with the override removed → must revert to the default.
	if err := tables.applyOverrides([]byte(`{}`)); err != nil {
		t.Fatalf("applyOverrides (clear): %v", err)
	}
	if got := tables.modelID("claude", TierMid); got != def {
		t.Fatalf("claude mid after removing override = %q, want default %q", got, def)
	}
}
