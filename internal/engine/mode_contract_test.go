package engine

import (
	"testing"

	"github.com/makewand/makewand/internal/model"
)

func TestModeContractBidirectionalMapping(t *testing.T) {
	tests := []struct {
		input           string
		wantCanonicalGo string
		wantPythonTier  string
		wantUsageMode   model.UsageMode
		wantOk          bool
	}{
		// Go canonical names
		{input: "fast", wantCanonicalGo: "fast", wantPythonTier: "fast", wantUsageMode: model.ModeFast, wantOk: true},
		{input: "balanced", wantCanonicalGo: "balanced", wantPythonTier: "standard", wantUsageMode: model.ModeBalanced, wantOk: true},
		{input: "power", wantCanonicalGo: "power", wantPythonTier: "deep", wantUsageMode: model.ModePower, wantOk: true},

		// Python tier aliases
		{input: "standard", wantCanonicalGo: "balanced", wantPythonTier: "standard", wantUsageMode: model.ModeBalanced, wantOk: true},
		{input: "deep", wantCanonicalGo: "power", wantPythonTier: "deep", wantUsageMode: model.ModePower, wantOk: true},

		// Case-insensitive & trimmed
		{input: "  STANDARD ", wantCanonicalGo: "balanced", wantPythonTier: "standard", wantUsageMode: model.ModeBalanced, wantOk: true},
		{input: "DEEP", wantCanonicalGo: "power", wantPythonTier: "deep", wantUsageMode: model.ModePower, wantOk: true},
		{input: " Balanced ", wantCanonicalGo: "balanced", wantPythonTier: "standard", wantUsageMode: model.ModeBalanced, wantOk: true},

		// Invalid values
		{input: "unknown", wantOk: false},
		{input: "", wantOk: false},
		{input: "ultra", wantOk: false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			goMode, ok1 := CanonicalGoMode(tt.input)
			if ok1 != tt.wantOk {
				t.Errorf("CanonicalGoMode(%q) ok = %v, want %v", tt.input, ok1, tt.wantOk)
			}
			if ok1 && goMode != tt.wantCanonicalGo {
				t.Errorf("CanonicalGoMode(%q) = %q, want %q", tt.input, goMode, tt.wantCanonicalGo)
			}

			pyTier, ok2 := ToPythonTier(tt.input)
			if ok2 != tt.wantOk {
				t.Errorf("ToPythonTier(%q) ok = %v, want %v", tt.input, ok2, tt.wantOk)
			}
			if ok2 && pyTier != tt.wantPythonTier {
				t.Errorf("ToPythonTier(%q) = %q, want %q", tt.input, pyTier, tt.wantPythonTier)
			}

			usageMode, ok3 := ToGoUsageMode(tt.input)
			if ok3 != tt.wantOk {
				t.Errorf("ToGoUsageMode(%q) ok = %v, want %v", tt.input, ok3, tt.wantOk)
			}
			if ok3 && usageMode != tt.wantUsageMode {
				t.Errorf("ToGoUsageMode(%q) = %v, want %v", tt.input, usageMode, tt.wantUsageMode)
			}
		})
	}
}
