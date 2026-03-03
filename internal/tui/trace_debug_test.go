package tui

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/model"
)

func TestSummarizeRouteEvent_Candidates(t *testing.T) {
	line := summarizeRouteEvent(model.TraceEvent{
		Event: "route_mode_candidates",
		Candidates: []model.TraceCandidate{
			{Name: "claude", ModelID: "claude-sonnet", ThompsonScore: 0.91, FailureRate: 0.02},
			{Name: "codex", ModelID: "codex-cli", ThompsonScore: 0.78, FailureRate: 0.01},
			{Name: "gemini", ModelID: "gemini-flash", ThompsonScore: 0.54, FailureRate: 0.10},
		},
	})
	if !strings.Contains(line, "Route candidates:") || !strings.Contains(line, "claude:claude-sonnet") {
		t.Fatalf("summary line missing expected candidates: %q", line)
	}
}

func TestSummarizeRouteEvent_Selected(t *testing.T) {
	line := summarizeRouteEvent(model.TraceEvent{
		Event:      "route_selected",
		Requested:  "claude",
		Selected:   "codex",
		IsFallback: true,
		ModelID:    "codex-cli",
	})
	if !strings.Contains(line, "claude -> codex") || !strings.Contains(line, "fallback=true") {
		t.Fatalf("summary line missing route selection info: %q", line)
	}
}

func TestRouteDebugState_Observe(t *testing.T) {
	state := newRouteDebugState()
	state.Observe(model.TraceEvent{
		Event:      "route_selected",
		Requested:  "gemini",
		Selected:   "claude",
		IsFallback: true,
	})
	if got := state.Summary(); got == "" {
		t.Fatal("route debug summary should not be empty after observe")
	}
}
