package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/engine"
)

func dependencyNetworkError() error {
	return fmt.Errorf("%w (npm error code ENOTFOUND): the host may be offline; this is an environment problem, not a candidate failure", engine.ErrDependencyNetworkUnavailable)
}

// assertBuildFinishedWithoutTests checks that the build ended with the tests
// skipped: no tests phase, no auto-fix, no quality penalty.
func assertBuildFinishedWithoutTests(t *testing.T, app App, cmd tea.Cmd) {
	t.Helper()
	if app.state == StateConfirmTests || app.pendingTestsPlan != nil || app.pendingDepsPlan != nil {
		t.Fatalf("state=%v pendingTests=%v pendingDeps=%v: the build went on towards the tests", app.state, app.pendingTestsPlan, app.pendingDepsPlan)
	}
	if step := app.progress.steps[stepTests]; step.Status != StepDone || step.Detail != testsDepsUnavailableDetail {
		t.Fatalf("tests step = %+v, want done/%q", step, testsDepsUnavailableDetail)
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("wizard phase = %v, want the build finished", app.wizard.Phase())
	}
	if cmd == nil {
		t.Fatal("no command after finishing the build")
	}
	if msg := cmd(); msg != (filesUpdatedMsg{}) {
		t.Fatalf("next message = %T %+v, want the build-complete refresh (no auto-fix)", msg, msg)
	}
}

// A dependency install that could not reach its registry ends the build:
// running the tests without dependencies would fail, be penalised and be
// handed to auto-fix for a problem no code change can fix.
func TestDepsRegistryUnreachableSkipsTests_FixN1R2(t *testing.T) {
	app := newBuildAppForDepsTest(t)
	app.pendingDepsPlan = &engine.ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}}

	model, cmd := app.Update(depsInstallMsg{result: &engine.ExecResult{ExitCode: 1, Stderr: "npm error code ENOTFOUND"}, err: dependencyNetworkError()})
	app = model.(App)
	assertBuildFinishedWithoutTests(t, app, cmd)
	if step := app.progress.steps[stepDeps]; step.Status != StepFailed || !strings.Contains(step.Detail, "could not reach the network") {
		t.Fatalf("deps step = %+v, want failed with the network error", step)
	}
}

// Other dependency install errors keep going to the tests, as before.
func TestDepsExecErrorStillRunsTests_FixN1R2(t *testing.T) {
	app := newBuildAppForDepsTest(t)
	app.pendingDepsPlan = &engine.ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}}

	model, _ := app.Update(depsInstallMsg{err: fmt.Errorf("verification sandbox failed to start: bwrap: setting up uid map: Permission denied")})
	app = model.(App)
	if app.state != StateConfirmTests || app.pendingTestsPlan == nil {
		t.Fatalf("state=%v pendingTests=%v, want the tests phase", app.state, app.pendingTestsPlan)
	}
}

// The auto-fix retry reports its dependency install error as a test run
// message; a registry outage there must also skip the tests instead of
// recording them as failed.
func TestAutoFixRetryRegistryUnreachableSkipsTests_FixN1R2(t *testing.T) {
	app := newBuildAppForDepsTest(t)

	model, cmd := app.Update(testRunMsg{result: &engine.ExecResult{ExitCode: 1, Stderr: "npm error code ENOTFOUND"}, err: dependencyNetworkError()})
	app = model.(App)
	assertBuildFinishedWithoutTests(t, app, cmd)
	if step := app.progress.steps[stepDeps]; step.Status != StepFailed {
		t.Fatalf("deps step = %+v, want failed", step)
	}
}
