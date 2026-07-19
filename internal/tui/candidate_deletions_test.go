package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

// TestRunCandidateSelection_SurfacesDeletionsFromDeleteOnlyCandidate ensures a
// candidate that only deletes files and returns empty content does not silently
// drop its deletions: they must reach the user as the deletion warning.
func TestRunCandidateSelection_SurfacesDeletionsFromDeleteOnlyCandidate(t *testing.T) {
	project := newTestlessCandidateProject(t)

	deleter := &fixedCandidateProvider{
		name:    "deleter",
		content: "", // delete-only: no FILE blocks, empty content
		mutate: func(ctx context.Context) error {
			dir, ok := model.WorkDirFromContext(ctx)
			if !ok {
				return context.Canceled
			}
			return os.Remove(filepath.Join(dir, "calc.go"))
		},
	}

	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"deleter": {Provider: deleter},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	selection := runCandidateSelection(
		context.Background(),
		router,
		project,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: "trim the project"}},
		"system",
	)

	if strings.TrimSpace(selection.content) != "" {
		t.Fatalf("selection.content = %q, want empty for a delete-only candidate", selection.content)
	}
	wantNote := fmt.Sprintf(i18n.Msg().AutomationCandidateDeletions, "calc.go")
	if !strings.Contains(selection.selectionNote, wantNote) {
		t.Fatalf("selectionNote = %q, want it to surface deletion warning %q", selection.selectionNote, wantNote)
	}
}

// TestHandleAIResponse_SurfacesDeleteOnlyWarningOnEmptyContent verifies the
// chat-flow consumer surfaces the deletion warning even on the empty-content
// early-return path (a delete-only candidate returns empty content plus an err).
func TestHandleAIResponse_SurfacesDeleteOnlyWarningOnEmptyContent(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	note := fmt.Sprintf(i18n.Msg().AutomationCandidateDeletions, "calc.go")
	updated, _ := app.Update(aiResponseMsg{
		provider:      "ensemble",
		selectionNote: note,
		err:           fmt.Errorf("no candidate provider produced a response"),
	})
	app = updated.(App)

	if !chatContainsMessage(app, note) {
		t.Fatalf("delete-only warning not surfaced on empty-content path: %+v", app.chat.messages)
	}
}

// TestChatFlow_DeleteOnlyCandidateSurfacesWarning drives the full autopilot
// chat-flow consumer path: a delete-only candidate (empty content) must still
// surface its deletion warning to the user through submitChatInput -> command ->
// handleAIResponse, rather than being dropped on the empty-content early return.
func TestChatFlow_DeleteOnlyCandidateSurfacesWarning(t *testing.T) {
	project := newTestlessCandidateProject(t)

	deleter := &fixedCandidateProvider{
		name:    "deleter",
		content: "", // delete-only: no FILE blocks, empty content
		mutate: func(ctx context.Context) error {
			dir, ok := model.WorkDirFromContext(ctx)
			if !ok {
				return context.Canceled
			}
			return os.Remove(filepath.Join(dir, "calc.go"))
		},
	}
	router := mustNewRouterFromConfig(t, model.RouterConfig{
		Providers: map[string]model.ProviderEntry{
			"deleter": {Provider: deleter},
		},
		UsageMode: "balanced",
	})
	router.SetMode(model.ModeBalanced)

	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeAuto
	app := *NewApp(ModeChat, cfg, "")
	app.router = router
	app.project = project

	m, cmd := app.submitChatInput("write code to trim the project")
	app = m.(App)
	if cmd == nil {
		t.Fatal("submitChatInput returned nil cmd")
	}
	updated, _ := app.Update(cmd())
	app = updated.(App)

	wantNote := fmt.Sprintf(i18n.Msg().AutomationCandidateDeletions, "calc.go")
	if !chatContainsMessage(app, wantNote) {
		t.Fatalf("chat did not surface deletion warning %q: %+v", wantNote, app.chat.messages)
	}
}
