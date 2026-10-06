package main

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

// go-engine-tui-cmd#7 (headless): discarded test edits and oversized files
// are announced on stderr, and weak/no-test verification is stated honestly.
func TestHeadlessSelectionNotices(t *testing.T) {
	notices := headlessSelectionNotices(engine.CandidateSelection{
		Content:       "x",
		Strength:      1,
		RestoredTests: []string{"math_test.go"},
		LargeFiles:    []string{"model.bin"},
	})
	joined := strings.Join(notices, "\n")
	for _, want := range []string{"math_test.go", "model.bin", i18n.Msg().AutomationCandidateWeakVerification} {
		if !strings.Contains(joined, want) {
			t.Fatalf("headless notices missing %q:\n%s", want, joined)
		}
	}
	noTests := strings.Join(headlessSelectionNotices(engine.CandidateSelection{Content: "x", NoTestsExecuted: true}), "\n")
	if !strings.Contains(noTests, i18n.Msg().AutomationCandidateNoTests) {
		t.Fatalf("headless notices do not say no tests ran: %q", noTests)
	}
}

// Automatic application requires configured independent acceptance; local
// checks alone still require approval.
func TestRootHelpExplainsAutopilotLimit(t *testing.T) {
	root := newRootCmd()
	if !strings.Contains(root.Long, "MAKEWAND_TRUSTED_ACCEPTANCE_FILE") || !strings.Contains(root.Long, "Strength 1 and still ask") {
		t.Fatalf("root help does not explain independent acceptance and local approval:\n%s", root.Long)
	}
	flag := root.PersistentFlags().Lookup("approval")
	if flag == nil || !strings.Contains(flag.Usage, "configured independent Strength-2 acceptance") || !strings.Contains(flag.Usage, "otherwise still asks") {
		t.Fatalf("--approval usage does not explain the autopilot limit: %+v", flag)
	}
}
