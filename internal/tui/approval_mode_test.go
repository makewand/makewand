package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

func chatContainsMessage(app App, needle string) bool {
	for _, msg := range app.chat.messages {
		if strings.Contains(msg.Content, needle) {
			return true
		}
	}
	return false
}

func TestApprovalModeCommand_UpdatesSessionMode(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	modelAfterCmd, cmd := app.submitChatInput("/approval safe")
	app = modelAfterCmd.(App)

	if cmd != nil {
		t.Fatal("/approval should be handled locally")
	}
	if got := app.currentApprovalMode(); got != config.ApprovalModeSafe {
		t.Fatalf("currentApprovalMode = %q, want %q", got, config.ApprovalModeSafe)
	}

	last := app.chat.messages[len(app.chat.messages)-1]
	want := fmt.Sprintf(i18n.Msg().ApprovalModeChanged, i18n.Msg().ApprovalModeSafe)
	if last.Content != want {
		t.Fatalf("last message = %q, want %q", last.Content, want)
	}
}

func TestHandleFilesExtracted_SafeModeAutoApprovesChatWrites(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeSafe
	app := *NewApp(ModeChat, cfg, "")

	project, err := engine.NewProject("safe-chat-write", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	app.project = project

	modelAfterFiles, cmd := app.handleFilesExtracted(filesExtractedMsg{
		files: []engine.ExtractedFile{{Path: "hello.txt", Content: "hello"}},
		phase: pendingPhaseChat,
	})
	app = modelAfterFiles.(App)

	if cmd == nil {
		t.Fatal("safe mode should auto-approve file writes")
	}
	if app.state == StateConfirmFiles {
		t.Fatal("state should not enter StateConfirmFiles in safe mode")
	}
	if !chatContainsMessage(app, fmt.Sprintf(i18n.Msg().ApprovalAutoWrite, 1)) {
		t.Fatalf("chat missing safe auto-approval status: %+v", app.chat.messages)
	}

	msg := cmd()
	confirm, ok := msg.(confirmFileWriteMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want confirmFileWriteMsg", msg)
	}
	if !confirm.confirmed {
		t.Fatal("confirmFileWriteMsg.confirmed = false, want true")
	}
}

func TestHandleFilesExtracted_AutopilotAutoApprovesVerifiedChatWrites(t *testing.T) {
	app, report := trustedAutopilotFixture(t)
	app.pendingWriteVerified = true
	app.pendingWriteAcceptance = report.Acceptance
	app.pendingWriteDigest = report.VerifiedDigest
	modelAfterFiles, cmd := app.handleFilesExtracted(filesExtractedMsg{files: report.VerifiedFiles, phase: pendingPhaseChat})
	app = modelAfterFiles.(App)
	if cmd == nil || app.state == StateConfirmFiles {
		t.Fatal("parent-issued independent acceptance should auto-approve autopilot writes")
	}
	if !chatContainsMessage(app, fmt.Sprintf(i18n.Msg().ApprovalAutoWriteAutopilot, 1)) {
		t.Fatalf("chat missing autopilot auto-approval status: %+v", app.chat.messages)
	}
	confirm, ok := cmd().(confirmFileWriteMsg)
	if !ok || !confirm.confirmed || !confirm.automatic {
		t.Fatal("automatic authorization was not retained")
	}
	_, write := app.handleFileWriteConfirm(confirm)
	result := write().(fileWriteCompleteMsg)
	if result.failed != 0 || result.written != 1 {
		t.Fatalf("valid parent approval could not be applied: %+v", result)
	}
}

func trustedAutopilotFixture(t *testing.T) (App, engine.CandidateVerification) {
	t.Helper()
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	if !engine.VerificationIsolationActive(engine.UnsafeHostExecAuthorization{}) {
		if os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
			t.Fatal("required real bubblewrap isolation is unavailable")
		}
		t.Skip("real bubblewrap isolation is unavailable")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
			t.Fatal("required python3 acceptance runtime is unavailable")
		}
		t.Skip("python3 is unavailable")
	}
	root := t.TempDir()
	t.Setenv("MAKEWAND_APPLY_STATE_DIR", filepath.Join(root, "private-apply"))
	project, err := engine.NewProject("autopilot", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFile("main.py", "print('before')\n"); err != nil {
		t.Fatal(err)
	}
	spec := engine.TrustedAcceptanceSpec{Schema: 1, Command: "python3", Args: []string{"-I", "main.py"}, Cases: []engine.TrustedAcceptanceCase{{Name: "behavior", Stdout: "42\n"}}}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "acceptance.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", path)
	report, err := project.EvaluateCandidateFiles(context.Background(), []engine.ExtractedFile{{Path: "main.py", Content: "print(42)\n"}})
	if err != nil || report.Acceptance == nil || report.Strength != 2 {
		t.Fatalf("real trusted fixture failed: %v %+v", err, report)
	}
	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeAuto
	app := *NewApp(ModeChat, cfg, "")
	app.project = project
	return app, report
}

func TestAutopilotNeverAutoApprovesMissingOrImportedReceipts(t *testing.T) {
	app, report := trustedAutopilotFixture(t)
	data, err := json.Marshal(report.Acceptance)
	if err != nil {
		t.Fatal(err)
	}
	var imported engine.TrustedAcceptanceRecord
	if err := json.Unmarshal(data, &imported); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []*engine.TrustedAcceptanceRecord{nil, &imported} {
		for _, phase := range []pendingPhaseType{pendingPhaseChat, pendingPhaseBuild, pendingPhaseFix} {
			app.pendingWriteVerified = true // an obsolete/fabricated bool is not proof
			app.pendingWriteAcceptance = receipt
			app.pendingWriteDigest = report.VerifiedDigest
			model, command := app.handleFilesExtracted(filesExtractedMsg{files: report.VerifiedFiles, phase: phase})
			if command != nil || model.(App).state != StateConfirmFiles {
				t.Fatalf("phase %v auto-approved a missing/imported receipt", phase)
			}
		}
	}
}

func TestAutopilotAutomaticWriteRejectsMissingAndStaleReceipts(t *testing.T) {
	for _, name := range []string{"missing", "stale baseline", "stale policy"} {
		t.Run(name, func(t *testing.T) {
			app, report := trustedAutopilotFixture(t)
			app.pendingFiles = report.VerifiedFiles
			app.pendingWriteVerified = true
			app.pendingWriteDigest = report.VerifiedDigest
			app.pendingWriteAcceptance = report.Acceptance
			switch name {
			case "missing":
				app.pendingWriteAcceptance = nil
			case "stale baseline":
				if err := app.project.WriteFile("outside-patch.txt", "external editor"); err != nil {
					t.Fatal(err)
				}
			case "stale policy":
				path := os.Getenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE")
				// This path is the isolated specification fixture created by this test.
				if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil { //nolint:gosec
					t.Fatal(err)
				}
			}
			_, command := app.handleFileWriteConfirm(confirmFileWriteMsg{confirmed: true, automatic: true})
			result := command().(fileWriteCompleteMsg)
			if result.written != 0 || result.failed != 1 {
				t.Fatalf("%s granted automatic application: %+v", name, result)
			}
			data, err := os.ReadFile(filepath.Join(app.project.Path, "main.py"))
			if err != nil || string(data) != "print('before')\n" {
				t.Fatalf("%s changed target: %q %v", name, data, err)
			}
		})
	}
}

func TestAutopilotExplicitHumanApprovalAllowsMissingReceipt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MAKEWAND_APPLY_STATE_DIR", filepath.Join(root, "private-apply"))
	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeAuto
	app := *NewApp(ModeChat, cfg, "")
	project, err := engine.NewProject("manual", root)
	if err != nil {
		t.Fatal(err)
	}
	app.project = project
	app.pendingFiles = []engine.ExtractedFile{{Path: "human.txt", Content: "human approved"}}
	_, command := app.handleFileWriteConfirm(confirmFileWriteMsg{confirmed: true})
	result := command().(fileWriteCompleteMsg)
	if result.failed != 0 || result.written != 1 {
		t.Fatalf("explicit human approval was blocked: %+v", result)
	}
}

// stubRestrictedExecIsolation fakes the host isolation probe so approval tests
// do not depend on bubblewrap being installed.
func stubRestrictedExecIsolation(t *testing.T, autoApprovable bool, reason error) {
	t.Helper()
	oldOK := restrictedExecAutoApprovable
	oldErr := restrictedExecIsolationError
	restrictedExecAutoApprovable = func(engine.UnsafeHostExecAuthorization) bool { return autoApprovable }
	restrictedExecIsolationError = func(engine.UnsafeHostExecAuthorization) error { return reason }
	t.Cleanup(func() {
		restrictedExecAutoApprovable = oldOK
		restrictedExecIsolationError = oldErr
	})
}

func TestSafeApproval_AutoRunsDepsAndTests(t *testing.T) {
	stubRestrictedExecIsolation(t, true, nil)
	app := newBuildAppForDepsTest(t)
	app.cfg.ApprovalMode = config.ApprovalModeSafe

	modelAfterStart, depsCmd := app.startDepsPhase()
	app = modelAfterStart.(App)
	if depsCmd == nil {
		t.Fatal("safe mode should auto-approve dependency install")
	}
	if app.state == StateConfirmDeps {
		t.Fatal("state should not enter StateConfirmDeps in safe mode")
	}
	if !app.pipeline.DepsApproved() {
		t.Fatal("deps should be marked approved in safe mode")
	}
	if !chatContainsMessage(app, i18n.Msg().ApprovalAutoDeps) {
		t.Fatalf("chat missing safe deps auto-approval message: %+v", app.chat.messages)
	}

	modelAfterDeps, testsCmd := app.Update(depsInstallMsg{result: &engine.ExecResult{ExitCode: 0, Stdout: "ok"}})
	app = modelAfterDeps.(App)
	if testsCmd == nil {
		t.Fatal("safe mode should auto-approve tests after deps")
	}
	if app.state == StateConfirmTests {
		t.Fatal("state should not enter StateConfirmTests in safe mode")
	}
	if !app.pipeline.TestsApproved() {
		t.Fatal("tests should be marked approved in safe mode")
	}
	if !chatContainsMessage(app, i18n.Msg().ApprovalAutoTests) {
		t.Fatalf("chat missing safe tests auto-approval message: %+v", app.chat.messages)
	}
}

func TestSafeApproval_PromptsForDepsWhenIsolationUnavailable(t *testing.T) {
	isolationErr := errors.New("candidate verification requires bubblewrap (bwrap)")
	stubRestrictedExecIsolation(t, false, isolationErr)
	app := newBuildAppForDepsTest(t)
	app.cfg.ApprovalMode = config.ApprovalModeSafe

	modelAfterStart, depsCmd := app.startDepsPhase()
	app = modelAfterStart.(App)
	if depsCmd != nil {
		t.Fatal("safe mode must not auto-run deps without sandbox isolation")
	}
	if app.state != StateConfirmDeps {
		t.Fatalf("state = %v, want %v", app.state, StateConfirmDeps)
	}
	if app.pipeline.DepsApproved() {
		t.Fatal("deps must not be marked approved without isolation")
	}
	wantNotice := fmt.Sprintf(i18n.Msg().ApprovalIsolationUnavailable, isolationErr)
	if !chatContainsMessage(app, wantNotice) {
		t.Fatalf("chat missing isolation notice %q: %+v", wantNotice, app.chat.messages)
	}
}

func TestAutopilotBuildFallsBackToManualWhenCandidateUnverified(t *testing.T) {
	app := newBuildAppForDepsTest(t)
	app.cfg.ApprovalMode = config.ApprovalModeAuto
	app.pendingWriteVerified = false

	modelAfterFiles, cmd := app.handleFilesExtracted(filesExtractedMsg{
		files: []engine.ExtractedFile{{Path: "main.go", Content: "package main"}},
		phase: pendingPhaseBuild,
	})
	app = modelAfterFiles.(App)

	if cmd != nil {
		t.Fatal("unverified autopilot build should not auto-confirm file writes")
	}
	if app.state != StateConfirmFiles {
		t.Fatalf("state = %v, want %v", app.state, StateConfirmFiles)
	}
	// The notice must explain the Strength-2 requirement honestly instead of
	// claiming that no candidate passed local verification.
	if !chatContainsMessage(app, i18n.Msg().AutopilotApprovalRequired) {
		t.Fatalf("chat missing autopilot approval explanation: %+v", app.chat.messages)
	}
	if chatContainsMessage(app, i18n.Msg().AutomationCandidateFallback) {
		t.Fatalf("chat claims no candidate passed local verification: %+v", app.chat.messages)
	}
}
