package router

import "testing"

func TestValidateStrategyTables_Valid(t *testing.T) {
	if err := validateStrategyTables(); err != nil {
		t.Fatalf("validateStrategyTables() error = %v, want nil", err)
	}
}

func TestValidateStrategyTables_MissingModelTableEntry(t *testing.T) {
	// Temporarily inject an unknown provider into strategyTable.
	orig := strategyTable[ModeFast][TaskCode]
	strategyTable[ModeFast][TaskCode] = strategyEntry{
		Tier:      TierCheap,
		Providers: []string{"gemini", "unknown-provider"},
	}
	defer func() { strategyTable[ModeFast][TaskCode] = orig }()

	if err := validateStrategyTables(); err == nil {
		t.Fatal("validateStrategyTables() = nil, want error for unknown provider")
	}
}

func TestValidateStrategyTables_MissingCostTableEntry(t *testing.T) {
	// Temporarily inject a model ID that has no cost table entry.
	orig := modelTable["gemini"][TierPremium]
	modelTable["gemini"][TierPremium] = "nonexistent-model-xyz"
	defer func() { modelTable["gemini"][TierPremium] = orig }()

	if err := validateStrategyTables(); err == nil {
		t.Fatal("validateStrategyTables() = nil, want error for missing cost entry")
	}
}

func TestValidateStrategyTables_PrimaryInFallbacks(t *testing.T) {
	// Temporarily corrupt a buildStrategyTable entry so Primary appears in Fallbacks.
	orig := buildStrategyTable[ModeBalanced][PhaseCode]
	buildStrategyTable[ModeBalanced][PhaseCode] = BuildStrategy{
		Tier:      TierMid,
		Primary:   "claude",
		Fallbacks: []string{"claude", "gemini"}, // Primary duplicated!
	}
	defer func() { buildStrategyTable[ModeBalanced][PhaseCode] = orig }()

	if err := validateStrategyTables(); err == nil {
		t.Fatal("validateStrategyTables() = nil, want error when Primary appears in Fallbacks")
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
