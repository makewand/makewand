package router

import "testing"

func TestValidateStrategyTables_Valid(t *testing.T) {
	if err := baseTables.validate(); err != nil {
		t.Fatalf("validate() error = %v, want nil", err)
	}
}

func TestValidateStrategyTables_MissingModelTableEntry(t *testing.T) {
	// Inject an unknown provider into a private copy's strategy table.
	tables := copyDefaultTables()
	tables.strategies[ModeFast][TaskCode] = strategyEntry{
		Tier:      TierCheap,
		Providers: []string{"gemini", "unknown-provider"},
	}

	if err := tables.validate(); err == nil {
		t.Fatal("validate() = nil, want error for unknown provider")
	}
}

func TestValidateStrategyTables_MissingCostTableEntry(t *testing.T) {
	// Inject a model ID that has no cost table entry into a private copy.
	tables := copyDefaultTables()
	tables.models["gemini"][TierPremium] = "nonexistent-model-xyz"

	if err := tables.validate(); err == nil {
		t.Fatal("validate() = nil, want error for missing cost entry")
	}
}

func TestValidateStrategyTables_PrimaryInFallbacks(t *testing.T) {
	// Corrupt a build strategy in a private copy so Primary appears in Fallbacks.
	tables := copyDefaultTables()
	tables.buildStrategies[ModeBalanced][PhaseCode] = BuildStrategy{
		Tier:      TierMid,
		Primary:   "claude",
		Fallbacks: []string{"claude", "gemini"}, // Primary duplicated!
	}

	if err := tables.validate(); err == nil {
		t.Fatal("validate() = nil, want error when Primary appears in Fallbacks")
	}
}

func TestSortCandidatesForMode_EconomyFastDegradesUnstableProvider(t *testing.T) {
	candidates := []candidate{
		{name: "unstable", access: AccessFree, order: 0, failureRate: 1.0, requests: 2, thompsonScore: 0.99},
		{name: "stable", access: AccessFree, order: 1, failureRate: 0.0, requests: 0, thompsonScore: 0.10},
	}

	sortCandidatesForMode(candidates, ModeFast)
	if candidates[0].name != "stable" {
		t.Fatalf("economy first candidate = %q, want %q (fast degrade threshold=2)", candidates[0].name, "stable")
	}
}

func TestSortCandidatesForMode_BalancedKeepsDefaultSampleThreshold(t *testing.T) {
	candidates := []candidate{
		{name: "unstable", access: AccessFree, order: 0, failureRate: 1.0, requests: 2, thompsonScore: 0.99},
		{name: "stable", access: AccessFree, order: 1, failureRate: 0.0, requests: 0, thompsonScore: 0.10},
	}

	sortCandidatesForMode(candidates, ModeBalanced)
	if candidates[0].name != "unstable" {
		t.Fatalf("balanced first candidate = %q, want %q (default threshold still 5)", candidates[0].name, "unstable")
	}
}

func TestSortCandidatesForMode_ColdStartPrefersStaticOrder(t *testing.T) {
	candidates := []candidate{
		{name: "preferred", access: AccessSubscription, order: 0, requests: 0, thompsonScore: 0.30},
		{name: "random-high", access: AccessSubscription, order: 1, requests: 0, thompsonScore: 0.95},
	}

	sortCandidatesForMode(candidates, ModeFast)
	if candidates[0].name != "preferred" {
		t.Fatalf("economy cold-start first candidate = %q, want %q (static order)", candidates[0].name, "preferred")
	}
}
