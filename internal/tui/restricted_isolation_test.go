package tui

import (
	"errors"
	"fmt"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

// TestRunDepsPlan_FailsClosedWhenIsolationUnavailable verifies the deps run site
// degrades to the manual-approval notice instead of executing a restricted plan
// on the host when sandbox isolation is unavailable.
func TestRunDepsPlan_FailsClosedWhenIsolationUnavailable(t *testing.T) {
	isoErr := errors.New("candidate verification requires bubblewrap (bwrap)")
	stubRestrictedExecIsolation(t, false, isoErr)

	app := newBuildAppForDepsTest(t)
	app.pipeline.SetDepsApproved(true)

	plan, err := app.project.DetectInstallPlan()
	if err != nil || plan == nil {
		t.Fatalf("DetectInstallPlan: plan=%v err=%v", plan, err)
	}
	app.pendingDepsPlan = plan
	app.pendingTestsPlan = &engine.ExecPlan{Kind: "tests", Command: "npm", Args: []string{"test"}}

	model, _ := app.runDepsPlan(plan)
	app = model.(App)

	wantNotice := fmt.Sprintf(i18n.Msg().ApprovalIsolationUnavailable, isoErr)
	if !chatContainsMessage(app, wantNotice) {
		t.Fatalf("chat missing isolation notice %q: %+v", wantNotice, app.chat.messages)
	}
	if app.progress.steps[stepDeps].Status != StepDone || app.progress.steps[stepDeps].Detail != depsInstallSkippedDetail {
		t.Fatalf("deps step = %+v, want done/%q", app.progress.steps[stepDeps], depsInstallSkippedDetail)
	}
	if app.progress.steps[stepTests].Status != StepDone {
		t.Fatalf("tests step status = %v, want done (skipped, not executed)", app.progress.steps[stepTests].Status)
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("wizard phase = %v, want WizardPhaseDone", app.wizard.Phase())
	}
}

// TestRunTestsPlan_FailsClosedWhenIsolationUnavailable verifies the tests run
// site fails closed with the isolation notice.
func TestRunTestsPlan_FailsClosedWhenIsolationUnavailable(t *testing.T) {
	isoErr := errors.New("candidate verification requires bubblewrap (bwrap)")
	stubRestrictedExecIsolation(t, false, isoErr)

	app := newBuildAppForDepsTest(t)
	app.pipeline.SetTestsApproved(true)

	plan := &engine.ExecPlan{Kind: "tests", Command: "npm", Args: []string{"test"}}
	app.pendingTestsPlan = plan

	model, _ := app.runTestsPlan(plan)
	app = model.(App)

	wantNotice := fmt.Sprintf(i18n.Msg().ApprovalIsolationUnavailable, isoErr)
	if !chatContainsMessage(app, wantNotice) {
		t.Fatalf("chat missing isolation notice %q: %+v", wantNotice, app.chat.messages)
	}
	if app.progress.steps[stepTests].Status != StepDone {
		t.Fatalf("tests step status = %v, want done (skipped, not executed)", app.progress.steps[stepTests].Status)
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("wizard phase = %v, want WizardPhaseDone", app.wizard.Phase())
	}
}

// TestAutoFixRetry_DoesNotReuseApprovalWhenIsolationUnavailable ensures an
// auto-fix retry does not reuse an earlier deps/tests approval to run generated
// commands on the host once isolation becomes unavailable.
func TestAutoFixRetry_DoesNotReuseApprovalWhenIsolationUnavailable(t *testing.T) {
	isoErr := errors.New("candidate verification requires bubblewrap (bwrap)")
	stubRestrictedExecIsolation(t, false, isoErr)

	app := newBuildAppForDepsTest(t)
	app.pipeline.SetDepsApproved(true)
	app.pipeline.SetTestsApproved(true)

	model, _ := app.handleAutoFixFileWriteComplete()
	app = model.(App)

	wantNotice := fmt.Sprintf(i18n.Msg().ApprovalIsolationUnavailable, isoErr)
	if !chatContainsMessage(app, wantNotice) {
		t.Fatalf("chat missing isolation notice %q: %+v", wantNotice, app.chat.messages)
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("wizard phase = %v, want WizardPhaseDone (retry must not run on host)", app.wizard.Phase())
	}
}

// TestAutoFixRetry_ManualModeRePromptsInsteadOfReusingApproval ensures an
// auto-fix retry in manual approval mode does NOT replay an earlier one-time
// deps/tests approval to run generated commands again: it clears the grant and
// re-prompts through the normal deps gate, even when isolation is available.
func TestAutoFixRetry_ManualModeRePromptsInsteadOfReusingApproval(t *testing.T) {
	stubRestrictedExecIsolation(t, true, nil)

	app := newBuildAppForDepsTest(t)
	app.cfg.ApprovalMode = config.ApprovalModeManual
	app.pipeline.SetDepsApproved(true)
	app.pipeline.SetTestsApproved(true)

	model, cmd := app.handleAutoFixFileWriteComplete()
	app = model.(App)

	if cmd != nil {
		t.Fatal("retry must not execute a restricted plan directly; it should wait for confirmation")
	}
	if app.state != StateConfirmDeps {
		t.Fatalf("state = %v, want StateConfirmDeps (retry must re-prompt, not replay approval)", app.state)
	}
	if app.pipeline.DepsApproved() || app.pipeline.TestsApproved() {
		t.Fatalf("stale approval not cleared: deps=%v tests=%v", app.pipeline.DepsApproved(), app.pipeline.TestsApproved())
	}
	if app.wizard.Phase() == WizardPhaseDone {
		t.Fatal("build must not complete: retry is waiting on re-confirmation")
	}
}

// TestAutoFixRetry_AutopilotWithIsolationStillFlows confirms the happy path is
// preserved: autopilot mode with isolation available auto-runs the retry
// (returns a command) without prompting, and does not replay a stale approval
// token (the grant is cleared and re-derived from the current isolation state).
func TestAutoFixRetry_AutopilotWithIsolationStillFlows(t *testing.T) {
	stubRestrictedExecIsolation(t, true, nil)

	app := newBuildAppForDepsTest(t)
	app.cfg.ApprovalMode = config.ApprovalModeAuto
	app.pipeline.SetDepsApproved(true)
	app.pipeline.SetTestsApproved(true)

	model, cmd := app.handleAutoFixFileWriteComplete()
	app = model.(App)

	if cmd == nil {
		t.Fatal("autopilot+isolation retry should return a run command (happy path preserved)")
	}
	if app.state == StateConfirmDeps || app.state == StateConfirmTests {
		t.Fatalf("autopilot must not prompt on retry, got state %v", app.state)
	}
	if app.pipeline.DepsApproved() || app.pipeline.TestsApproved() {
		t.Fatalf("stale approval not cleared before retry: deps=%v tests=%v", app.pipeline.DepsApproved(), app.pipeline.TestsApproved())
	}
}

// TestRunDepsPlan_RunsWhenIsolationAvailable confirms the guard is a no-op when
// isolation (or the opt-in) is available: the deps command is still returned.
func TestRunDepsPlan_RunsWhenIsolationAvailable(t *testing.T) {
	stubRestrictedExecIsolation(t, true, nil)

	app := newBuildAppForDepsTest(t)
	app.cfg.ApprovalMode = config.ApprovalModeManual
	plan, err := app.project.DetectInstallPlan()
	if err != nil || plan == nil {
		t.Fatalf("DetectInstallPlan: plan=%v err=%v", plan, err)
	}
	app.pendingDepsPlan = plan

	model, cmd := app.runDepsPlan(plan)
	app = model.(App)
	if cmd == nil {
		t.Fatal("runDepsPlan should return the deps command when isolation is available")
	}
	if app.wizard.Phase() == WizardPhaseDone {
		t.Fatal("runDepsPlan should not complete the build when isolation is available")
	}
}
