package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

func newBuildAppForDepsTest(t *testing.T) App {
	t.Helper()

	cfg := config.DefaultConfig()
	appPtr := NewApp(ModeNew, cfg, "")
	app := *appPtr

	project, err := engine.NewProject("deps-confirm-test", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := project.WriteFile("package.json", `{"name":"demo","scripts":{"test":"echo ok"}}`); err != nil {
		t.Fatalf("WriteFile(package.json): %v", err)
	}
	app.project = project
	app.mode = ModeChat
	app.wizard.SetPhase(WizardPhaseBuild)

	msg := i18n.Msg()
	app.progress.SetSteps([]ProgressStep{
		{Label: msg.ProgressAnalyzing, Status: StepDone},
		{Label: msg.ProgressCreating, Status: StepDone},
		{Label: msg.ProgressReviewing, Status: StepDone},
		{Label: msg.ProgressInstallingDeps, Status: StepPending},
		{Label: msg.ProgressTesting, Status: StepPending},
	})

	return app
}

func TestDepsConfirm_DeclinePath(t *testing.T) {
	app := newBuildAppForDepsTest(t)

	model, cmd := app.startDepsPhase()
	app = model.(App)
	if cmd != nil {
		t.Fatal("startDepsPhase should not return command before user confirmation")
	}
	if app.state != StateConfirmDeps {
		t.Fatal("state should be StateConfirmDeps after entering deps phase")
	}
	last := app.chat.messages[len(app.chat.messages)-1].Content
	if !strings.Contains(last, depsInstallConfirmPrompt) {
		t.Fatalf("deps confirm prompt missing base text: %q", last)
	}
	if !strings.Contains(last, "npm install") {
		t.Fatalf("deps confirm prompt should include planned command, got: %q", last)
	}

	model, cmd = app.handleDepsConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	app = model.(App)
	if cmd == nil {
		t.Fatal("handleDepsConfirmKey(n) should return confirmation command")
	}

	msg := cmd()
	model, _ = app.Update(msg)
	app = model.(App)

	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("wizard phase = %v, want %v", app.wizard.Phase(), WizardPhaseDone)
	}
	if app.progress.steps[stepDeps].Status != StepDone || app.progress.steps[stepDeps].Detail != depsInstallSkippedDetail {
		t.Fatalf("deps step = %+v, want done/%q", app.progress.steps[stepDeps], depsInstallSkippedDetail)
	}
	if app.progress.steps[stepTests].Status != StepDone || app.progress.steps[stepTests].Detail != testsSkippedDetail {
		t.Fatalf("tests step = %+v, want done/%q", app.progress.steps[stepTests], testsSkippedDetail)
	}
}

func TestDepsAndTestsConfirm_AcceptPath(t *testing.T) {
	app := newBuildAppForDepsTest(t)

	model, cmd := app.startDepsPhase()
	app = model.(App)
	if cmd != nil {
		t.Fatal("startDepsPhase should not return command before user confirmation")
	}

	model, cmd = app.handleDepsConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = model.(App)
	if cmd == nil {
		t.Fatal("handleDepsConfirmKey(y) should return confirmation command")
	}

	confirmMsg := cmd()
	model, depsCmd := app.Update(confirmMsg)
	app = model.(App)
	if !app.depsInstallApproved {
		t.Fatal("depsInstallApproved should be true after accepting install")
	}
	if depsCmd == nil {
		t.Fatal("expected deps install command after accepting confirmation")
	}

	// Simulate successful dependency install without running external tools.
	model, testsPhaseCmd := app.Update(depsInstallMsg{result: &engine.ExecResult{ExitCode: 0, Stdout: "ok"}})
	app = model.(App)
	if testsPhaseCmd != nil {
		t.Fatal("startTestsPhase should wait for test confirmation before returning a command")
	}
	if app.state != StateConfirmTests {
		t.Fatal("state should be StateConfirmTests after deps success")
	}
	last := app.chat.messages[len(app.chat.messages)-1].Content
	if !strings.Contains(last, "Run project tests now?") {
		t.Fatalf("tests confirm prompt missing text: %q", last)
	}
	if !strings.Contains(last, "npm test") {
		t.Fatalf("tests confirm prompt should include planned command, got: %q", last)
	}

	model, cmd = app.handleTestsConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	app = model.(App)
	if cmd == nil {
		t.Fatal("handleTestsConfirmKey(y) should return confirmation command")
	}

	testsConfirmMsg := cmd()
	model, testsCmd := app.Update(testsConfirmMsg)
	app = model.(App)
	if !app.testsRunApproved {
		t.Fatal("testsRunApproved should be true after accepting test run")
	}
	if testsCmd == nil {
		t.Fatal("expected tests command after confirmation")
	}

	// Simulate successful tests without running external tools.
	model, _ = app.Update(testRunMsg{result: &engine.ExecResult{ExitCode: 0, Stdout: "ok"}})
	app = model.(App)
	if app.progress.steps[stepTests].Status != StepDone {
		t.Fatalf("tests step status = %v, want done", app.progress.steps[stepTests].Status)
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("wizard phase = %v, want %v", app.wizard.Phase(), WizardPhaseDone)
	}
}

func TestTestsConfirm_DeclinePath(t *testing.T) {
	app := newBuildAppForDepsTest(t)
	app.depsInstallApproved = true

	model, cmd := app.startTestsPhase()
	app = model.(App)
	if cmd != nil {
		t.Fatal("startTestsPhase should wait for user confirmation")
	}
	if app.state != StateConfirmTests {
		t.Fatal("state should be StateConfirmTests")
	}

	model, cmd = app.handleTestsConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	app = model.(App)
	if cmd == nil {
		t.Fatal("handleTestsConfirmKey(n) should return confirmation command")
	}

	msg := cmd()
	model, _ = app.Update(msg)
	app = model.(App)
	if app.progress.steps[stepTests].Status != StepDone {
		t.Fatalf("tests step status = %v, want done", app.progress.steps[stepTests].Status)
	}
	if app.progress.steps[stepTests].Detail != testsRunSkippedDetail {
		t.Fatalf("tests step detail = %q, want %q", app.progress.steps[stepTests].Detail, testsRunSkippedDetail)
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("wizard phase = %v, want %v", app.wizard.Phase(), WizardPhaseDone)
	}
}
