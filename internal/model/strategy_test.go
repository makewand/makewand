package model

import "testing"

func TestValidateStrategyTables_Valid(t *testing.T) {
	if err := validateStrategyTables(); err != nil {
		t.Fatalf("validateStrategyTables() error = %v, want nil", err)
	}
}

func TestValidateStrategyTables_MissingModelTableEntry(t *testing.T) {
	// Temporarily inject an unknown provider into strategyTable.
	orig := strategyTable[ModeFree][TaskCode]
	strategyTable[ModeFree][TaskCode] = strategyEntry{
		Tier:      TierCheap,
		Providers: []string{"gemini", "unknown-provider"},
	}
	defer func() { strategyTable[ModeFree][TaskCode] = orig }()

	if err := validateStrategyTables(); err == nil {
		t.Fatal("validateStrategyTables() = nil, want error for unknown provider")
	}
}

func TestValidateStrategyTables_MissingCostTableEntry(t *testing.T) {
	// Temporarily inject a model ID that has no cost table entry.
	orig := modelTable["ollama"][TierPremium]
	modelTable["ollama"][TierPremium] = "nonexistent-model-xyz"
	defer func() { modelTable["ollama"][TierPremium] = orig }()

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
