package tui

import (
	"fmt"
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
	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeAuto
	app := *NewApp(ModeChat, cfg, "")

	project, err := engine.NewProject("autopilot-chat-write", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	app.project = project
	app.pendingWriteVerified = true

	modelAfterFiles, cmd := app.handleFilesExtracted(filesExtractedMsg{
		files: []engine.ExtractedFile{{Path: "hello.txt", Content: "hello"}},
		phase: pendingPhaseChat,
	})
	app = modelAfterFiles.(App)

	if cmd == nil {
		t.Fatal("autopilot should auto-approve verified chat file writes")
	}
	if app.state == StateConfirmFiles {
		t.Fatal("state should not enter StateConfirmFiles for verified autopilot chat writes")
	}
	if !chatContainsMessage(app, fmt.Sprintf(i18n.Msg().ApprovalAutoWriteAutopilot, 1)) {
		t.Fatalf("chat missing autopilot auto-approval status: %+v", app.chat.messages)
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

func TestSafeApproval_AutoRunsDepsAndTests(t *testing.T) {
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
	if !chatContainsMessage(app, i18n.Msg().AutomationCandidateFallback) {
		t.Fatalf("chat missing autopilot fallback message: %+v", app.chat.messages)
	}
}
