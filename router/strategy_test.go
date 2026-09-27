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

func TestParseUsageModeAndAliases(t *testing.T) {
	tests := []struct {
		input    string
		wantMode UsageMode
		wantOk   bool
		wantStr  string
		wantPy   string
	}{
		{input: "fast", wantMode: ModeFast, wantOk: true, wantStr: "fast", wantPy: "fast"},
		{input: "balanced", wantMode: ModeBalanced, wantOk: true, wantStr: "balanced", wantPy: "standard"},
		{input: "power", wantMode: ModePower, wantOk: true, wantStr: "power", wantPy: "deep"},
		// Aliases
		{input: "standard", wantMode: ModeBalanced, wantOk: true, wantStr: "balanced", wantPy: "standard"},
		{input: "deep", wantMode: ModePower, wantOk: true, wantStr: "power", wantPy: "deep"},
		{input: " STANDARD ", wantMode: ModeBalanced, wantOk: true, wantStr: "balanced", wantPy: "standard"},
		{input: " DEEP ", wantMode: ModePower, wantOk: true, wantStr: "power", wantPy: "deep"},
		// Invalid
		{input: "unknown", wantMode: 0, wantOk: false},
		{input: "", wantMode: 0, wantOk: false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			mode, ok := ParseUsageMode(tt.input)
			if ok != tt.wantOk {
				t.Fatalf("ParseUsageMode(%q) ok = %v, want %v", tt.input, ok, tt.wantOk)
			}
			if ok {
				if mode != tt.wantMode {
					t.Errorf("ParseUsageMode(%q) = %v, want %v", tt.input, mode, tt.wantMode)
				}
				if mode.String() != tt.wantStr {
					t.Errorf("String() = %q, want %q", mode.String(), tt.wantStr)
				}
				if py := UsageModeToPythonTier(mode); py != tt.wantPy {
					t.Errorf("UsageModeToPythonTier(%v) = %q, want %q", mode, py, tt.wantPy)
				}
				norm, normOk := NormalizeModeOrTier(tt.input)
				if !normOk || norm != tt.wantStr {
					t.Errorf("NormalizeModeOrTier(%q) = (%q, %v), want (%q, true)", tt.input, norm, normOk, tt.wantStr)
				}
			}
		})
	}
}
