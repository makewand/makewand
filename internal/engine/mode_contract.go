package engine

import (
	"github.com/makewand/makewand/internal/model"
)

// ModeContract documents and provides bidirectional conversion between Go usage modes
// (fast, balanced, power) and Python orchestrator tiers (fast, standard, deep).
//
// Mapping specification:
//   Go "fast"     <--> Python "fast"
//   Go "balanced" <--> Python "standard"
//   Go "power"    <--> Python "deep"

// CanonicalGoMode returns the canonical Go mode string ("fast", "balanced", "power")
// for any recognized Go mode or Python tier.
func CanonicalGoMode(input string) (string, bool) {
	return model.NormalizeModeOrTier(input)
}

// ToPythonTier converts a Go usage mode or Python tier string into Python's canonical tier string
// ("fast", "standard", "deep").
func ToPythonTier(input string) (string, bool) {
	m, ok := model.ParseUsageMode(input)
	if !ok {
		return "", false
	}
	return model.UsageModeToPythonTier(m), true
}

// ToGoUsageMode converts a Go usage mode or Python tier string into Go's model.UsageMode.
func ToGoUsageMode(input string) (model.UsageMode, bool) {
	return model.ParseUsageMode(input)
}

// UsageModeToPythonTier converts a Go UsageMode into Python's canonical tier string.
func UsageModeToPythonTier(m model.UsageMode) string {
	return model.UsageModeToPythonTier(m)
}
