package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
)

func pendingApprovalApp(t *testing.T) (App, *config.Config) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", state)
	t.Setenv("MAKEWAND_APPLY_STATE_DIR", filepath.Join(state, "apply"))
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeAuto
	return *NewApp(ModeChat, cfg, workspace), cfg
}

func queuePendingApproval(t *testing.T, app App) App {
	t.Helper()
	app.pendingWriteAcceptance = &engine.TrustedAcceptanceRecord{Schema: 1, SpecDigest: "never-import-this-authority"}
	model, command := app.handleFilesExtracted(filesExtractedMsg{files: []engine.ExtractedFile{{Path: "answer.txt", Content: "after"}}, phase: pendingPhaseBuild})
	app = model.(App)
	if command != nil || app.state != StateConfirmFiles || app.pendingPersistence == nil {
		t.Fatalf("candidate did not reach a durable manual approval: state=%v command=%v", app.state, command)
	}
	return app
}

func approvedPendingWrite(t *testing.T, app App) (App, tea.Cmd) {
	t.Helper()
	model, decision := app.handleApproveCommand()
	app = model.(App)
	if decision == nil {
		t.Fatal("human decision did not queue a write")
	}
	message := decision().(confirmFileWriteMsg)
	if message.automatic || !message.confirmed {
		t.Fatal("recovered decision gained automatic authority")
	}
	model, write := app.handleFileWriteConfirm(message)
	app = model.(App)
	if write == nil {
		t.Fatal("explicit approval did not produce a transaction")
	}
	return app, write
}

func TestPendingApprovalNewAppRequiresFreshHumanReview(t *testing.T) {
	app, cfg := pendingApprovalApp(t)
	queuePendingApproval(t, app)
	restored := *NewApp(ModeChat, cfg, app.project.Path)
	if restored.state != StateConfirmFiles || !restored.pendingRecovery || len(restored.pendingFiles) != 1 {
		t.Fatalf("new App did not restore the payload: %+v", restored.pendingApproval)
	}
	if cfg.ApprovalMode != config.ApprovalModeAuto || restored.pendingWriteVerified || restored.pendingWriteAcceptance != nil || restored.pendingWriteDigest != "" {
		t.Fatal("recovery changed user preferences or restored old process authority")
	}
	for _, phase := range []pendingPhaseType{pendingPhaseChat, pendingPhaseBuild, pendingPhaseFix} {
		if restored.shouldAutoApproveFileWrites(phase) {
			t.Fatal("recovered candidate is automatically approvable")
		}
	}
	model, command := restored.handleFileWriteConfirm(confirmFileWriteMsg{confirmed: true, automatic: true})
	restored = model.(App)
	if command != nil || len(restored.pendingFiles) == 0 {
		t.Fatal("automatic message bypassed recovered manual approval")
	}
	restored, command = approvedPendingWrite(t, restored)
	completed := command().(fileWriteCompleteMsg)
	if completed.failed != 0 || completed.written != 1 {
		t.Fatalf("manual transaction failed: %+v", completed)
	}
	model, next := restored.handleFileWriteComplete(completed)
	restored = model.(App)
	if next != nil {
		t.Fatal("recovery automatically resumed the old build command chain")
	}
	if restored.pendingRecovery || restored.pendingPersistence != nil || cfg.ApprovalMode != config.ApprovalModeAuto {
		t.Fatal("single recovered decision changed subsequent approval preferences")
	}
	fresh := NewApp(ModeChat, cfg, app.project.Path)
	if fresh.pendingApproval != nil {
		t.Fatal("completed approval restored again")
	}
	data, _ := os.ReadFile(filepath.Join(app.project.Path, "answer.txt"))
	if string(data) != "after" {
		t.Fatal("human-reviewed payload was not applied")
	}
}

func TestPendingApprovalRecoveredKeyboardDenialIsDurable(t *testing.T) {
	app, cfg := pendingApprovalApp(t)
	queuePendingApproval(t, app)
	restored := *NewApp(ModeChat, cfg, app.project.Path)
	model, command := restored.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	restored = model.(App)
	if command != nil || restored.pendingApproval != nil || len(restored.pendingFiles) != 0 {
		t.Fatal("keyboard denial did not terminate recovered evidence")
	}
	fresh := NewApp(ModeChat, cfg, app.project.Path)
	if fresh.pendingApproval != nil || cfg.ApprovalMode != config.ApprovalModeAuto {
		t.Fatal("denied recovery resumed or modified the approval setting")
	}
	data, _ := os.ReadFile(filepath.Join(app.project.Path, "answer.txt"))
	if string(data) != "before" {
		t.Fatal("denial mutated the project")
	}
}

func TestPendingApprovalQueuedWriteRechecksBaselineUnderApplyLock(t *testing.T) {
	app, cfg := pendingApprovalApp(t)
	dependency := filepath.Join(app.project.Path, "node_modules", "fixture.js")
	if err := os.MkdirAll(filepath.Dir(dependency), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dependency, []byte("original dependency"), 0o600); err != nil {
		t.Fatal(err)
	}
	queuePendingApproval(t, app)
	restored := *NewApp(ModeChat, cfg, app.project.Path)
	_, write := approvedPendingWrite(t, restored)
	// Change an execution input after the confirmation has produced its Cmd.
	if err := os.WriteFile(dependency, []byte("external edit after approval"), 0o600); err != nil {
		t.Fatal(err)
	}
	completed := write().(fileWriteCompleteMsg)
	if completed.failed != 1 || completed.written != 0 || !strings.Contains(strings.Join(completed.errors, ";"), "baseline changed") {
		t.Fatalf("queued write ignored late input drift: %+v", completed)
	}
	data, _ := os.ReadFile(filepath.Join(app.project.Path, "answer.txt"))
	if string(data) != "before" {
		t.Fatal("stale candidate overwrote the project")
	}
}

func TestPendingApprovalTwoAppsCannotUseRevokedQueuedAuthorization(t *testing.T) {
	for _, action := range []string{"deny", "supersede"} {
		t.Run(action, func(t *testing.T) {
			app, cfg := pendingApprovalApp(t)
			queuePendingApproval(t, app)
			first := *NewApp(ModeChat, cfg, app.project.Path)
			second := *NewApp(ModeChat, cfg, app.project.Path)
			_, write := approvedPendingWrite(t, first)
			if action == "deny" {
				_, _ = second.handleDenyCommand()
			} else {
				_, err := second.project.SavePendingApproval(context.Background(), []engine.ExtractedFile{{Path: "answer.txt", Content: "newer candidate"}},
					string(approvalFileWrite), int(pendingPhaseChat), "Review new candidate", "one file", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			completed := write().(fileWriteCompleteMsg)
			if completed.failed != 1 || completed.written != 0 || !strings.Contains(strings.Join(completed.errors, ";"), "denied or superseded") {
				t.Fatalf("revoked authorization was still applied: %+v", completed)
			}
			data, _ := os.ReadFile(filepath.Join(app.project.Path, "answer.txt"))
			if string(data) != "before" {
				t.Fatal("revoked payload mutated the workspace")
			}
		})
	}
}

func TestPendingApprovalRecoveryStopsOnExternalEdit(t *testing.T) {
	app, cfg := pendingApprovalApp(t)
	queuePendingApproval(t, app)
	if err := os.WriteFile(filepath.Join(app.project.Path, "answer.txt"), []byte("third-party edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := NewApp(ModeChat, cfg, app.project.Path)
	if fresh.pendingApproval != nil || len(fresh.pendingFiles) != 0 || !chatContainsMessage(*fresh, "baseline changed") {
		t.Fatal("conflicting pending approval did not fail closed with a retained diagnostic")
	}
	data, _ := os.ReadFile(filepath.Join(app.project.Path, "answer.txt"))
	if string(data) != "third-party edit" {
		t.Fatal("NewApp modified an external edit")
	}
}

func TestPendingApprovalProcessHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_TUI_PENDING_CHILD") != "1" {
		return
	}
	cfg := config.DefaultConfig()
	cfg.ApprovalMode = config.ApprovalModeManual
	app := *NewApp(ModeChat, cfg, os.Getenv("MAKEWAND_TUI_PENDING_PROJECT"))
	model, cmd := app.handleFilesExtracted(filesExtractedMsg{files: []engine.ExtractedFile{{Path: "answer.txt", Content: "child candidate"}}, phase: pendingPhaseChat})
	app = model.(App)
	if cmd != nil || app.pendingPersistence == nil || app.state != StateConfirmFiles {
		t.Fatal("child candidate was not durably awaiting review")
	}
	os.Exit(87)
}

func TestPendingApprovalIndependentProcessRestoresWithoutChatShutdown(t *testing.T) {
	app, cfg := pendingApprovalApp(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPendingApprovalProcessHelper$") //nolint:gosec // G204: execute only this fixed test binary and one hard-exit fixture.
	cmd.Env = append(os.Environ(), "MAKEWAND_TUI_PENDING_CHILD=1", "MAKEWAND_TUI_PENDING_PROJECT="+app.project.Path)
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 87 {
		t.Fatalf("child did not stop at pending approval: %v %s", err, output)
	}
	fresh := NewApp(ModeChat, cfg, app.project.Path)
	if !fresh.pendingRecovery || len(fresh.pendingFiles) != 1 || fresh.pendingFiles[0].Content != "child candidate" || fresh.pendingWriteAcceptance != nil {
		t.Fatal("new process lost payload or retained parent authority")
	}
	data, _ := os.ReadFile(filepath.Join(app.project.Path, "answer.txt"))
	if string(data) != "before" {
		t.Fatal("startup executed the saved candidate")
	}
}

func TestPendingApprovalOldCompletionCannotTerminateNewCandidate(t *testing.T) {
	app, cfg := pendingApprovalApp(t)
	queuePendingApproval(t, app)
	app = *NewApp(ModeChat, cfg, app.project.Path)
	app, write := approvedPendingWrite(t, app)
	completed := write().(fileWriteCompleteMsg)
	if completed.failed != 0 {
		t.Fatalf("first candidate failed: %+v", completed)
	}
	// A new approval arrived before the old Cmd's completion was processed.
	model, command := app.handleFilesExtracted(filesExtractedMsg{files: []engine.ExtractedFile{{Path: "answer.txt", Content: "new candidate"}}, phase: pendingPhaseBuild})
	app = model.(App)
	if command != nil || app.pendingPersistence == nil {
		t.Fatal("new approval was not queued")
	}
	newID := app.pendingPersistence.ID
	model, command = app.handleFileWriteComplete(completed)
	app = model.(App)
	if command != nil || app.pendingPersistence == nil || app.pendingPersistence.ID != newID || app.state != StateConfirmFiles {
		t.Fatal("old completion terminated the newer approval")
	}
	fresh := NewApp(ModeChat, cfg, app.project.Path)
	if fresh.pendingPersistence == nil || fresh.pendingPersistence.ID != newID || fresh.pendingFiles[0].Content != "new candidate" {
		t.Fatal("newer pending payload was marked completed by the older transaction")
	}
}

func TestPendingApprovalRecoveredCommandCompletionStopsTheOldChain(t *testing.T) {
	app, cfg := pendingApprovalApp(t)
	record, err := app.project.SavePendingApproval(context.Background(), nil, "tests", int(pendingPhaseBuild), "Review tests", "offline plan",
		&engine.ExecPlan{Kind: "tests", Command: "offline"})
	if err != nil {
		t.Fatal(err)
	}
	app = *NewApp(ModeChat, cfg, app.project.Path)
	if app.state != StateConfirmTests || !app.pendingRecovery {
		t.Fatal("unapproved command details were not recovered for manual review")
	}
	model, decision := app.handleApproveCommand()
	app = model.(App)
	if decision == nil {
		t.Fatal("recovered command could not receive a fresh human decision")
	}
	// A completed reviewed command must end its record and stop the old chain.
	model, next := app.handleTestRun(testRunMsg{approval: record, recovered: true, result: &engine.ExecResult{ExitCode: 0, Stdout: "offline completed"}})
	app = model.(App)
	if next != nil || app.pendingRecovery || app.pendingPersistence != nil || cfg.ApprovalMode != config.ApprovalModeAuto {
		t.Fatal("recovered command resumed automation or permanently changed approval policy")
	}
	if fresh := NewApp(ModeChat, cfg, app.project.Path); fresh.pendingApproval != nil {
		t.Fatal("completed recovered command remained pending")
	}
}

func TestPendingApprovalCommandMutationBeforeDecisionIsRejected(t *testing.T) {
	for _, kind := range []approvalKind{approvalDeps, approvalTests} {
		t.Run(string(kind), func(t *testing.T) {
			app, _ := pendingApprovalApp(t)
			plan := &engine.ExecPlan{Kind: string(kind), Command: "python", Args: []string{"-c", "print('approved')"}}
			if kind == approvalDeps {
				app.pendingDepsPlan = plan
			} else {
				app.pendingTestsPlan = plan
			}
			app.setPendingApproval(kind, "Review command", "offline plan")
			plan.Args[1] = "print('unapproved')"
			model, command := app.handleApproveCommand()
			app = model.(App)
			if command != nil || !chatContainsMessage(app, "command payload changed") {
				t.Fatal("changed command received the old user decision")
			}
		})
	}
}

func TestPendingApprovalQueuedCommandMutationNeverExecutes(t *testing.T) {
	for _, kind := range []approvalKind{approvalDeps, approvalTests} {
		for _, mutation := range []string{"argument", "command"} {
			t.Run(string(kind)+"/"+mutation, func(t *testing.T) {
				app, _ := pendingApprovalApp(t)
				t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
				audited := 0
				auth := engine.UnsafeHostExecAuthorization{Acknowledged: true, Source: "offline-pending-fixture", Audit: func(engine.UnsafeHostExecEvent) { audited++ }}
				app.hostExecAuth = auth
				app.project.SetUnsafeHostExecAuthorization(auth)
				plan := &engine.ExecPlan{Kind: string(kind), Command: "python", Args: []string{"-I", "-c", "from pathlib import Path; Path('executed.txt').write_text('approved')"}}
				if kind == approvalDeps {
					app.pendingDepsPlan = plan
				} else {
					app.pendingTestsPlan = plan
				}
				app.setPendingApproval(kind, "Review command", "offline plan")
				if !app.persistApprovalDecision(true) {
					t.Fatal("fixture could not authorize its fixed plan")
				}
				var command tea.Cmd
				if kind == approvalDeps {
					_, command = app.runDepsPlan(plan)
				} else {
					_, command = app.runTestsPlan(plan)
				}
				if command == nil {
					t.Fatal("fixture plan did not reach its queued execution boundary")
				}
				if mutation == "argument" {
					plan.Args[2] = "from pathlib import Path; Path('executed.txt').write_text('unapproved')"
				} else {
					plan.Command = "python3"
				}
				var executionErr error
				if kind == approvalDeps {
					executionErr = command().(depsInstallMsg).err
				} else {
					executionErr = command().(testRunMsg).err
				}
				if executionErr == nil || !strings.Contains(executionErr.Error(), "command payload changed") || audited != 0 {
					t.Fatalf("queued mutation reached command execution: error=%v audit=%d", executionErr, audited)
				}
				if _, err := os.Stat(filepath.Join(app.project.Path, "executed.txt")); !os.IsNotExist(err) {
					t.Fatalf("unapproved process executed: %v", err)
				}
			})
		}
	}
}
