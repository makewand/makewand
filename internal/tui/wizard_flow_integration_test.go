package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

func TestWizardBuildsProjectFromTemplateWithoutPTY(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DefaultModel = "private"
	cfg.AnalysisModel = "private"
	cfg.CodingModel = "private"
	cfg.ReviewModel = "private"

	provider := &wizardRetryStubProvider{
		name: "private",
		responses: []string{
			"Plan:\n- Build a simple page\n- No dependencies\n- No tests\n",
			"--- FILE: index.html ---\n```\n<html><body><h1>hello</h1></body></html>\n```",
		},
	}

	app := *NewApp(ModeNew, cfg, "")
	app.router = newWizardFlowRouter(t, cfg, provider)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	runDir := t.TempDir()
	if err := os.Chdir(runDir); err != nil {
		t.Fatalf("Chdir(%s): %v", runDir, err)
	}
	defer func() {
		_ = os.Chdir(origWD)
	}()

	enter := tea.KeyMsg{Type: tea.KeyEnter}

	var cmd tea.Cmd
	app, cmd = updateWizardApp(t, app, enter)
	if app.wizard.Phase() != WizardPhasePlan {
		t.Fatalf("phase after template enter = %v, want %v", app.wizard.Phase(), WizardPhasePlan)
	}
	app = runWizardCmds(t, app, cmd)

	app, cmd = updateWizardApp(t, app, enter)
	if cmd != nil {
		t.Fatal("plan confirmation should not return a command")
	}
	if app.wizard.Phase() != WizardPhaseConfirm {
		t.Fatalf("phase after plan confirm = %v, want %v", app.wizard.Phase(), WizardPhaseConfirm)
	}

	app, cmd = updateWizardApp(t, app, enter)
	if app.project == nil {
		t.Fatal("project should be created during build confirmation")
	}
	app = runWizardCmds(t, app, cmd)

	projectFile := filepath.Join(runDir, "blog-project", "index.html")
	data, err := os.ReadFile(projectFile)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", projectFile, err)
	}
	if !strings.Contains(string(data), "<h1>hello</h1>") {
		t.Fatalf("generated file content = %q, want hello heading", string(data))
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("final wizard phase = %v, want %v", app.wizard.Phase(), WizardPhaseDone)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.calls)
	}
}

func newWizardFlowRouter(t *testing.T, cfg *config.Config, provider model.Provider) *model.Router {
	t.Helper()

	r := model.NewRouter(cfg)
	if err := r.RegisterProvider("private", provider, model.AccessSubscription); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	return r
}

func updateWizardApp(t *testing.T, app App, msg tea.Msg) (App, tea.Cmd) {
	t.Helper()

	modelValue, cmd := app.Update(msg)
	next, ok := modelValue.(App)
	if !ok {
		t.Fatalf("Update() returned %T, want App", modelValue)
	}
	return next, cmd
}

func runWizardCmds(t *testing.T, app App, cmd tea.Cmd) App {
	t.Helper()

	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		nextCmd := queue[0]
		queue = queue[1:]
		if nextCmd == nil {
			continue
		}

		msg := nextCmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}

		var followUp tea.Cmd
		app, followUp = updateWizardApp(t, app, msg)
		queue = append(queue, followUp)
	}

	return app
}
