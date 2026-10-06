package tui

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

// go-engine-tui-cmd#7: edits to existing test files are dropped from the
// delivered candidate; the TUI must say so instead of losing them silently.
func TestCandidateSelectionNote_ListsDiscardedTestEdits(t *testing.T) {
	note := candidateSelectionNote(engine.CandidateSelection{
		Content:       "--- FILE: math.go ---\n```\npackage m\n```",
		Provider:      "alpha",
		Strength:      1,
		RestoredTests: []string{"math_test.go", "package.json"},
	})
	want := "math_test.go, package.json"
	if !strings.Contains(note, want) || !strings.Contains(note, strings.SplitN(i18n.Msg().AutomationCandidateRestoredTests, "%s", 2)[0]) {
		t.Fatalf("selection note does not announce the discarded test edits: %q", note)
	}
}

func TestCandidateSelectionNote_ListsOversizedFiles(t *testing.T) {
	note := candidateSelectionNote(engine.CandidateSelection{
		Content:    "x",
		Strength:   1,
		LargeFiles: []string{"model.bin"},
	})
	if !strings.Contains(note, "model.bin") {
		t.Fatalf("selection note does not mention the oversized file: %q", note)
	}
}

// R04: a local pass is self-reported by candidate code; the note must say the
// result was not independently verified.
func TestCandidateSelectionNote_WeakPassSaysNotIndependentlyVerified(t *testing.T) {
	note := candidateSelectionNote(engine.CandidateSelection{Content: "x", Strength: 1})
	if note != i18n.Msg().AutomationCandidateWeakVerification {
		t.Fatalf("note = %q", note)
	}
	prev := i18n.GetLanguage()
	defer i18n.SetLanguage(prev)
	for _, lang := range []string{"en", "zh"} {
		i18n.SetLanguage(lang)
		text := strings.ToLower(i18n.Msg().AutomationCandidateWeakVerification)
		if !strings.Contains(text, "not independently verified") && !strings.Contains(text, "未经独立验证") {
			t.Errorf("[%s] weak-verification text does not state the pass is unverified: %q", lang, text)
		}
	}
}

// go-engine-tui-cmd#10: a candidate checked without any test must be
// presented as "no tests executed", not as a (weak) pass.
func TestCandidateSelectionNote_NoTestsExecuted(t *testing.T) {
	note := candidateSelectionNote(engine.CandidateSelection{Content: "x", NoTestsExecuted: true})
	if note != i18n.Msg().AutomationCandidateNoTests {
		t.Fatalf("note = %q, want the no-tests notice", note)
	}
}

// Local checks alone give Strength 1; configured independent acceptance is
// required for automatic application at Strength 2.
func TestAutopilotDescriptionsAreHonest(t *testing.T) {
	for _, s := range approvalSlashCommandSuggestions {
		if s.Command == "/approval autopilot" && strings.Contains(strings.ToLower(s.Description), "auto-select verified") {
			t.Fatalf("autopilot suggestion still promises automatic selection: %q", s.Description)
		}
	}
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	next, _ := app.submitChatInput("/approval autopilot")
	app = next.(App)
	if !chatContainsMessage(app, i18n.Msg().ApprovalModeAutopilotNote) {
		t.Fatalf("switching to autopilot does not explain that approval is still required: %+v", app.chat.messages)
	}
	for _, text := range []string{i18n.Msg().ApprovalModeAutopilotNote, i18n.Msg().AutopilotApprovalRequired} {
		if !strings.Contains(text, "2") || !strings.Contains(text, "1") {
			t.Fatalf("autopilot text must state the Strength 2 requirement vs the Strength 1 ceiling: %q", text)
		}
	}
}
