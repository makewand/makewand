package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

func (a *App) pendingPersistenceNotice(err error) {
	a.chat.AddMessage(ChatMessage{Role: "system", Content: fmt.Sprintf(i18n.Msg().ApprovalRecoveryStopped, err)})
}

func (a *App) persistPendingFiles() bool {
	if a.project == nil || len(a.pendingFiles) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record, err := a.project.SavePendingApproval(ctx, a.pendingFiles, string(approvalFileWrite), int(a.pendingPhase),
		fmt.Sprintf(i18n.Msg().FileConfirmWrite, len(a.pendingFiles)), pendingWriteDetails(len(a.pendingFiles)), nil)
	if err != nil {
		a.pendingPersistenceNotice(err)
		a.pendingFiles = nil
		a.state = StateIdle
		return false
	}
	a.pendingPersistence = record
	a.pendingRecovery = false
	return true
}

func (a *App) persistApprovalRequest() bool {
	if a.project == nil || a.pendingApproval == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := a.pendingApproval
	if request.Kind == approvalFileWrite && a.pendingPersistence != nil && a.pendingPersistence.Kind == string(approvalFileWrite) {
		if err := a.project.ValidatePendingApproval(ctx, a.pendingPersistence, a.pendingFiles); err != nil {
			a.pendingPersistenceNotice(err)
			a.pendingFiles = nil
			a.pendingApproval = nil
			a.state = StateIdle
			return false
		}
		return true
	}
	var plan *engine.ExecPlan
	var files []engine.ExtractedFile
	switch request.Kind {
	case approvalFileWrite:
		files = a.pendingFiles
	case approvalDeps:
		plan = a.pendingDepsPlan
	case approvalTests:
		plan = a.pendingTestsPlan
	}
	record, err := a.project.SavePendingApproval(ctx, files, string(request.Kind), int(a.pendingPhase), request.Title, request.Details, plan)
	if err != nil {
		a.pendingPersistenceNotice(err)
		a.pendingFiles = nil
		a.pendingApproval = nil
		a.state = StateIdle
		return false
	}
	a.pendingPersistence = record
	return true
}

func (a *App) persistApprovalDecision(approved bool) bool {
	if a.project == nil || a.pendingPersistence == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state := "denied"
	if approved {
		files := a.pendingFiles
		if a.pendingPersistence.Kind != string(approvalFileWrite) {
			files = nil
			plan := a.pendingTestsPlan
			if a.pendingPersistence.Kind == string(approvalDeps) {
				plan = a.pendingDepsPlan
			}
			if !engine.PendingApprovalPlanMatches(a.pendingPersistence, plan) {
				a.pendingPersistenceNotice(fmt.Errorf("pending approval command payload changed after sealing"))
				return false
			}
		}
		if err := a.project.ValidatePendingApproval(ctx, a.pendingPersistence, files); err != nil {
			a.pendingPersistenceNotice(err)
			return false
		}
		state = "authorized"
	}
	if err := a.project.TransitionPendingApproval(ctx, a.pendingPersistence, state, "explicit user decision"); err != nil {
		a.pendingPersistenceNotice(err)
		return false
	}
	if !approved {
		a.pendingPersistence = nil
		a.pendingRecovery = false
	}
	return true
}

func (a *App) beginPendingWrite(msg confirmFileWriteMsg) (*engine.PendingApproval, error) {
	if a.pendingRecovery && msg.automatic {
		return nil, errors.New(i18n.Msg().ApprovalRecoveredManual)
	}
	if a.pendingPersistence == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.project.ValidatePendingApproval(ctx, a.pendingPersistence, a.pendingFiles); err != nil {
		return nil, err
	}
	if err := a.project.TransitionPendingApproval(ctx, a.pendingPersistence, "authorized", "file transaction starting"); err != nil {
		return nil, err
	}
	return a.pendingPersistence, nil
}

func (a *App) finishPendingWrite(msg fileWriteCompleteMsg) bool {
	if msg.approval == nil {
		return false
	}
	stale := a.pendingPersistence != nil && a.pendingPersistence.ID != msg.approval.ID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state := "applied"
	if msg.failed > 0 {
		state = "failed"
	}
	project := &engine.Project{Path: msg.approval.Workspace}
	if err := project.TransitionPendingApproval(ctx, msg.approval, state, strings.Join(msg.errors, "; ")); err != nil {
		a.pendingPersistenceNotice(err)
	}
	if a.pendingPersistence != nil && a.pendingPersistence.ID == msg.approval.ID {
		a.pendingPersistence = nil
		a.pendingRecovery = false
	}
	return stale
}

func (a *App) restorePendingApproval() {
	if a.project == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record, err := a.project.LoadPendingApproval(ctx)
	if err != nil {
		a.pendingPersistenceNotice(err)
		return
	}
	if record == nil {
		return
	}
	if record.RecoveredApplied {
		a.chat.AddMessage(ChatMessage{Role: "status", Content: i18n.Msg().ApprovalRecoveredApplied})
		return
	}
	a.pendingPersistence = record
	a.pendingRecovery = true
	a.pendingWriteVerified = false
	a.pendingWriteDigest = ""
	a.pendingWriteAcceptance = nil
	// The saved phase remains in the evidence. Recovery resumes only a single
	// manual decision and must never resume the old build/fix command chain.
	a.pendingPhase = pendingPhaseChat
	a.pendingFiles = append([]engine.ExtractedFile(nil), record.Files...)
	a.pendingApproval = &approvalRequest{Kind: approvalKind(record.Kind), Title: record.Title,
		Details: record.Details + "\n" + i18n.Msg().ApprovalRecoveredReview}
	switch approvalKind(record.Kind) {
	case approvalFileWrite:
		a.state = StateConfirmFiles
	case approvalDeps:
		a.pendingDepsPlan = record.Plan
		a.state = StateConfirmDeps
	case approvalTests:
		a.pendingTestsPlan = record.Plan
		a.state = StateConfirmTests
	default:
		a.pendingApproval = nil
		a.pendingPersistence = nil
		a.pendingFiles = nil
		a.pendingPersistenceNotice(fmt.Errorf("unsupported saved approval kind %q", record.Kind))
		return
	}
	a.chat.AddMessage(ChatMessage{Role: "system", Content: a.pendingApprovalSummary()})
}

func (a *App) finishPendingExecution(record *engine.PendingApproval, result *engine.ExecResult, executionErr error, recovered bool) bool {
	if record == nil {
		return false
	}
	stale := a.pendingPersistence != nil && a.pendingPersistence.ID != record.ID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state, detail := "applied", execResultSummary(result)
	if executionErr != nil || (result != nil && result.ExitCode != 0) {
		state = "failed"
		if executionErr != nil {
			detail = executionErr.Error()
		}
	}
	project := &engine.Project{Path: record.Workspace}
	if err := project.TransitionPendingApproval(ctx, record, state, detail); err != nil {
		a.pendingPersistenceNotice(err)
	}
	if a.pendingPersistence != nil && a.pendingPersistence.ID == record.ID {
		a.pendingPersistence = nil
		a.pendingRecovery = false
		a.pendingDepsPlan = nil
		a.pendingTestsPlan = nil
		a.state = StateIdle
	}
	if recovered {
		a.chat.AddMessage(ChatMessage{Role: "status", Content: i18n.Msg().ApprovalRecoveredCommandDone + "\n" + detail})
	}
	return recovered || stale
}
