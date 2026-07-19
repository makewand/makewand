package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

const depsInstallSkippedDetail = "skipped for safety"
const testsSkippedDetail = "skipped (dependency install not approved)"
const testsRunSkippedDetail = "skipped by user"

// --- AI response handling ---

func (a App) handleAIResponse(msg aiResponseMsg) (tea.Model, tea.Cmd) {
	// Surface the selection note (which carries delete-only warnings) before the
	// error branch so a delete-only candidate that produced no writable content
	// still shows its deletion warning to the user.
	if msg.selectionNote != "" {
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: msg.selectionNote,
		})
	}

	if msg.err != nil {
		a.recordKnownUsage(msg.provider, msg.cost, msg.inputTokens, msg.outputTokens)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: chatErrorContent(msg.err),
		})
		a.chat.SetStreaming(false)
		a.state = StateIdle
		a.activity.Reset()
		return a, nil
	}

	a.chat.AddMessage(ChatMessage{
		Role:     "assistant",
		Content:  msg.content,
		Provider: msg.provider,
		Cost:     msg.cost,
	})

	// Track cost with token details
	isSub := a.router.IsSubscription(msg.provider)
	a.cost.AddWithTokens(msg.provider, msg.cost, msg.inputTokens, msg.outputTokens, isSub)
	a = a.noteHostCLIExec(msg.provider)
	a.chat.SetStreaming(false)
	a.state = StateIdle
	a.activity.Reset()

	// Record the code provider for cross-model review
	if a.wizard.Phase() == WizardPhaseBuild && a.pipeline.CodeProvider() == "" {
		a.pipeline.SetCodeProvider(msg.provider)
	}

	// Check for files in non-streaming responses (build phase uses Chat, not ChatStream).
	if a.project != nil {
		phase := pendingPhaseChat
		if a.wizard.Phase() == WizardPhaseBuild {
			phase = pendingPhaseBuild
		}
		var result engine.ParseResult
		if phase == pendingPhaseBuild {
			result = engine.ParseFilesBestEffort(msg.content)
		} else if engine.ContainsFiles(msg.content) {
			result = engine.ParseFiles(msg.content)
		}
		if len(result.Files) > 0 {
			a.pendingWriteVerified = msg.verified
			return a, func() tea.Msg {
				return filesExtractedMsg{files: result.Files, phase: phase}
			}
		}
	}

	a.pendingWriteVerified = false

	return a, nil
}

// chatErrorContent formats a generation error for display as a chat system
// message. When routing fails closed in untrusted-repo mode it substitutes a
// clear, actionable message for the terse sentinel error; all other errors are
// shown verbatim.
func chatErrorContent(err error) string {
	if errors.Is(err, model.ErrNoUntrustedSafeProvider) {
		return i18n.Msg().RepoTrustNoSafeProvider
	}
	return fmt.Sprintf("Error: %s", err)
}

func (a *App) recordKnownUsage(provider string, cost float64, inputTokens, outputTokens int) {
	provider = strings.TrimSpace(provider)
	if a.cost == nil || provider == "" || (cost == 0 && inputTokens == 0 && outputTokens == 0) {
		return
	}
	isSubscription := a.router != nil && a.router.IsSubscription(provider)
	a.cost.AddWithTokens(provider, cost, inputTokens, outputTokens, isSubscription)
}

// noteHostCLIExec shows a one-time, session-scoped notice the first time a
// host-executing CLI provider generates a response, reminding the user that
// generation runs on the host with their environment/credentials — unlike the
// sandboxed verification stage. Informational only; see SECURITY.md.
func (a App) noteHostCLIExec(provider string) App {
	provider = strings.TrimSpace(provider)
	if a.hostCLINoticeShown || provider == "" || a.router == nil || !a.router.IsSubscription(provider) {
		return a
	}
	a.hostCLINoticeShown = true
	wd := "."
	if a.project != nil && strings.TrimSpace(a.project.Path) != "" {
		wd = a.project.Path
	}
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf(i18n.Msg().HostCLIExecNotice, provider, wd),
	})
	return a
}

// --- File extraction and writing ---

func (a App) handleFilesExtracted(msg filesExtractedMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	a.pendingFiles = msg.files
	a.pendingPhase = msg.phase

	if msg.phase == pendingPhaseBuild {
		a.progress.SetStepDetail(stepCode, fmt.Sprintf(m.ProgressFilesFound, len(msg.files)))
		if !a.shouldUseAutopilotCandidates() || a.pendingWriteVerified {
			// Build phase: auto-confirm file writing unless autopilot verification failed.
			return a, func() tea.Msg {
				return confirmFileWriteMsg{confirmed: true}
			}
		}
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: m.AutomationCandidateFallback,
		})
	}

	if a.shouldAutoApproveFileWrites(msg.phase) {
		a.addAutoApprovalStatus(a.autoApprovedWriteStatus(len(msg.files)))
		return a, func() tea.Msg {
			return confirmFileWriteMsg{confirmed: true}
		}
	}

	// Review, fix, and chat phases: ask for confirmation
	a.state = StateConfirmFiles
	a.setPendingApproval(
		approvalFileWrite,
		fmt.Sprintf(m.FileConfirmWrite, len(msg.files)),
		pendingWriteDetails(len(msg.files)),
	)
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: approvalPrompt(fmt.Sprintf(m.FileConfirmWrite, len(msg.files)), pendingWriteDetails(len(msg.files))),
	})
	return a, nil
}

func (a App) handleFileConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	key := strings.ToLower(msg.String())

	switch key {
	case "y", "enter":
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmFileWriteMsg{confirmed: true}
		}
	case "n", "esc":
		a.state = StateIdle
		a.clearPendingApproval()
		a.pendingFiles = nil
		a.pendingWriteVerified = false
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: m.FileCancelled,
		})
		return a, nil
	}
	return a, nil
}

func (a App) handleDepsConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := strings.ToLower(msg.String())
	switch key {
	case "y", "enter":
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmDepsInstallMsg{confirmed: true}
		}
	case "n", "esc":
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmDepsInstallMsg{confirmed: false}
		}
	}
	return a, nil
}

func (a App) handleTestsConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := strings.ToLower(msg.String())
	switch key {
	case "y", "enter":
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmTestsRunMsg{confirmed: true}
		}
	case "n", "esc":
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmTestsRunMsg{confirmed: false}
		}
	}
	return a, nil
}

func (a App) handleFileWriteConfirm(msg confirmFileWriteMsg) (tea.Model, tea.Cmd) {
	if !msg.confirmed || a.project == nil || len(a.pendingFiles) == 0 {
		a.pendingFiles = nil
		return a, nil
	}

	files := a.pendingFiles
	proj := a.project
	a.pendingFiles = nil

	return a, func() tea.Msg {
		checkpoint, checkpointErr := proj.CheckpointFiles(files)
		var written, failed int
		var errors []string

		for _, f := range files {
			if err := proj.WriteFile(f.Path, f.Content); err != nil {
				failed++
				errors = append(errors, fmt.Sprintf("%s: %s", f.Path, err))
			} else {
				written++
			}
		}
		if failed > 0 && checkpointErr == nil && checkpoint != nil {
			if err := checkpoint.Restore(); err != nil {
				errors = append(errors, fmt.Sprintf("rollback: %s", err))
			}
		}
		if checkpointErr != nil {
			errors = append(errors, fmt.Sprintf("checkpoint: %s", checkpointErr))
		}

		return fileWriteCompleteMsg{written: written, failed: failed, errors: errors}
	}
}

func (a App) handleFileWriteComplete(msg fileWriteCompleteMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	var cmds []tea.Cmd

	if msg.written > 0 {
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: fmt.Sprintf(m.FileWriteCount, msg.written),
		})
	}

	for _, errStr := range msg.errors {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.ErrFileWrite, errStr, ""),
		})
	}

	// Refresh file tree synchronously so downstream review sees the latest files.
	a.refreshProjectFiles()
	a.pendingWriteVerified = false

	if a.pendingPhase == pendingPhaseReview {
		// Review fix files written — continue to deps phase
		a.progress.SetStepStatus(stepReview, StepDone)
		a.progress.SetStepDetail(stepReview, fmt.Sprintf(m.ProgressReviewApplied, msg.written, a.pipeline.ReviewProvider()))
		a.pipeline.OnReviewFixesWritten()
		depsModel, depsCmd := a.startDepsPhase()
		a = depsModel.(App)
		if depsCmd != nil {
			cmds = append(cmds, depsCmd)
		}
		return a, tea.Batch(cmds...)
	}

	if a.pendingPhase == pendingPhaseFix {
		// Auto-fix files written — trigger retry of deps + tests
		return a.handleAutoFixFileWriteComplete()
	}

	if a.pendingPhase == pendingPhaseBuild {
		a = a.applyBudgetRoutingPolicy()

		// Update progress: generate code → done
		a.progress.SetStepStatus(stepCode, StepDone)
		a.progress.SetStepDetail(stepCode, fmt.Sprintf(m.ProgressFilesWritten, msg.written))

		// Ask the pipeline whether to review or skip.
		codeProvider := a.pipeline.CodeProvider()
		a.pipeline.SetAvailableProviders(len(a.router.Available()))
		action := a.pipeline.OnCodeWritten()

		if action.Kind == engine.ActionSkipReview {
			a.progress.SetStepStatus(stepReview, StepDone)
			a.progress.SetStepDetail(stepReview, fmt.Sprintf("skipped (%s)", action.SkipReason))
			depsModel, depsCmd := a.startDepsPhase()
			a = depsModel.(App)
			if depsCmd != nil {
				cmds = append(cmds, depsCmd)
			}
			return a, tea.Batch(cmds...)
		}

		a.progress.SetStepStatus(stepReview, StepRunning)

		// In Power mode the ensemble handles provider selection internally.
		// In other modes, pre-determine the review provider for the progress display.
		if a.router.Mode() == model.ModePower {
			a.progress.SetStepDetail(stepReview, fmt.Sprintf("Power: %s → ensemble", codeProvider))
		} else {
			reviewProvider := a.router.BuildProviderFor(model.PhaseReview)
			if reviewProvider == codeProvider {
				result, err := a.router.RouteProvider(reviewProvider, model.PhaseReview, codeProvider)
				if err == nil {
					reviewProvider = result.Actual
				}
			}
			a.pipeline.SetReviewProvider(reviewProvider)
			a.progress.SetStepDetail(stepReview, fmt.Sprintf(m.ProgressCrossModel, codeProvider, reviewProvider))
		}

		proj := a.project
		router := a.router
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		a.cancelAI = cancel
		cmds = append(cmds, func() tea.Msg {
			defer cancel()

			// Collect files for review (cap at ~32 KB to avoid CLI timeout).
			const maxReviewBytes = 32 * 1024
			var fileContents strings.Builder
			if proj != nil {
				for _, f := range proj.Files {
					if f.IsDir {
						continue
					}
					content, err := proj.ReadFile(f.Path)
					if err != nil {
						continue
					}
					entry := fmt.Sprintf("--- FILE: %s ---\n```\n%s\n```\n\n", f.Path, content)
					if fileContents.Len()+len(entry) > maxReviewBytes {
						fmt.Fprintf(&fileContents, "(... %d more files omitted for size)\n", len(proj.Files))
						break
					}
					fileContents.WriteString(entry)
				}
			}

			reviewPrompt := buildReviewUserPrompt(fileContents.String())
			messages := []model.Message{{Role: "user", Content: reviewPrompt}}

			// ChatBest: Power mode uses ensemble+judge; others use the single review provider.
			// Exclude the code provider so cross-model constraint is always enforced.
			content, usage, route, err := router.ChatBest(ctx, model.PhaseReview, messages, codeReviewSystemPrompt, codeProvider)
			provider := providerForUsage(usage, route)
			if err != nil {
				return codeReviewMsg{
					provider:     provider,
					cost:         usage.Cost,
					inputTokens:  usage.InputTokens,
					outputTokens: usage.OutputTokens,
					err:          err,
				}
			}

			return codeReviewMsg{
				content:      content,
				provider:     provider,
				cost:         usage.Cost,
				inputTokens:  usage.InputTokens,
				outputTokens: usage.OutputTokens,
				hasIssues:    !isLGTMResponse(content),
			}
		})
	}

	return a, tea.Batch(cmds...)
}

// --- Code Review ---

func (a App) handleCodeReview(msg codeReviewMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()

	if msg.err != nil {
		a.recordKnownUsage(msg.provider, msg.cost, msg.inputTokens, msg.outputTokens)
		// Review error is non-fatal — skip review and continue to deps
		a.progress.SetStepStatus(stepReview, StepDone)
		a.progress.SetStepDetail(stepReview, fmt.Sprintf("skipped: %s", msg.err))
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Review skipped: %s", msg.err),
		})
		a.pipeline.OnReviewComplete(engine.ReviewError)
		return a.startDepsPhase()
	}

	// Track cost
	isSub := a.router.IsSubscription(msg.provider)
	a.cost.AddWithTokens(msg.provider, msg.cost, msg.inputTokens, msg.outputTokens, isSub)

	if !msg.hasIssues {
		// LGTM — no changes needed
		a.progress.SetStepStatus(stepReview, StepDone)
		a.progress.SetStepDetail(stepReview, m.ProgressReviewLGTM)
		a.chat.AddMessage(ChatMessage{
			Role:     "assistant",
			Content:  m.ProgressReviewLGTM,
			Provider: msg.provider,
			Cost:     msg.cost,
		})
		a.pipeline.OnReviewComplete(engine.ReviewLGTM)
		return a.startDepsPhase()
	}

	// Review found issues — parse fix files and ask for confirmation
	a.chat.AddMessage(ChatMessage{
		Role:     "assistant",
		Content:  msg.content,
		Provider: msg.provider,
		Cost:     msg.cost,
	})

	result := engine.ParseFilesBestEffort(msg.content)
	if len(result.Files) == 0 {
		// Review had comments but no file fixes — treat as LGTM
		a.progress.SetStepStatus(stepReview, StepDone)
		a.progress.SetStepDetail(stepReview, fmt.Sprintf(m.ProgressReviewDone, msg.provider))
		a.pipeline.OnReviewComplete(engine.ReviewNoFixFiles)
		return a.startDepsPhase()
	}

	a.pipeline.OnReviewComplete(engine.ReviewHasIssues)
	a.progress.SetStepDetail(stepReview, m.ProgressReviewFixing)
	return a, func() tea.Msg {
		return filesExtractedMsg{files: result.Files, phase: pendingPhaseReview}
	}
}

// startDepsPhase starts the dependency installation phase.
func (a App) startDepsPhase() (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	a.progress.SetStepStatus(stepDeps, StepRunning)
	a.progress.SetStepDetail(stepDeps, m.ProgressInstallingDeps)

	if a.project == nil {
		return a.buildComplete()
	}

	plan, err := a.project.DetectInstallPlan()
	if err != nil {
		emitExecTrace(a.router, "pipeline.exec.plan_error", "deps", nil, nil, nil, err, "failed to detect dependency install command")
		a.progress.SetStepStatus(stepDeps, StepFailed)
		a.progress.SetStepDetail(stepDeps, err.Error())
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.BuildDepsDetectFailed, err),
		})
		return a.buildComplete()
	}
	a.pendingDepsPlan = plan

	if plan == nil {
		emitExecTrace(a.router, "pipeline.exec.not_detected", "deps", nil, nil, nil, nil, "no dependency install command detected")
		a.progress.SetStepStatus(stepDeps, StepDone)
		a.progress.SetStepDetail(stepDeps, m.ProgressNoDeps)
		return a.startTestsPhase()
	}
	emitExecTrace(a.router, "pipeline.exec.plan_detected", "deps", plan, nil, nil, nil, "detected dependency install plan")

	if !a.pipeline.DepsApproved() && a.shouldAutoApproveRestrictedPlan(plan) {
		a.pipeline.SetDepsApproved(true)
		emitExecTrace(a.router, "pipeline.exec.confirmed", "deps", plan, boolPtr(true), nil, nil, "dependency install auto-approved by safe mode")
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: m.ApprovalAutoDeps + "\n" + plannedCommandDetails(plan.DisplayCommand()),
		})
		return a.runDepsPlan(plan)
	}

	if !a.pipeline.DepsApproved() {
		a.state = StateConfirmDeps
		a.setPendingApproval(
			approvalDeps,
			m.ApprovalDepsConfirm,
			plannedCommandDetails(plan.DisplayCommand()),
		)
		emitExecTrace(a.router, "pipeline.exec.confirm_requested", "deps", plan, nil, nil, nil, "waiting for dependency install confirmation")
		if notice := a.restrictedPlanIsolationNotice(); notice != "" {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: notice,
			})
		}
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: approvalPrompt(m.ApprovalDepsConfirm, plannedCommandDetails(plan.DisplayCommand())),
		})
		return a, nil
	}

	return a.runDepsPlan(plan)
}

func (a App) runDepsPlan(plan *engine.ExecPlan) (tea.Model, tea.Cmd) {
	if a.project == nil || plan == nil {
		return a, nil
	}
	a.clearPendingApproval()
	// Fail closed: RunRestrictedPlan will refuse to run generated commands on the
	// host when sandbox isolation is unavailable and MAKEWAND_UNSAFE_HOST_EXEC is
	// not set. Detect that here so we skip deps + tests with a clear notice
	// instead of surfacing a raw error.
	if notice := restrictedPlanBlockedNotice(); notice != "" {
		emitExecTrace(a.router, "pipeline.exec.skipped", "deps", plan, nil, nil, nil, "sandbox isolation unavailable")
		emitExecTrace(a.router, "pipeline.exec.skipped", "tests", a.pendingTestsPlan, nil, nil, nil, "sandbox isolation unavailable")
		a.progress.SetStepStatus(stepDeps, StepDone)
		a.progress.SetStepDetail(stepDeps, depsInstallSkippedDetail)
		a.progress.SetStepStatus(stepTests, StepDone)
		a.progress.SetStepDetail(stepTests, testsSkippedDetail)
		a.chat.AddMessage(ChatMessage{Role: "system", Content: notice})
		a.pendingDepsPlan = nil
		a.pendingTestsPlan = nil
		return a.buildComplete()
	}
	proj := a.project
	planValue := *plan
	emitExecTrace(a.router, "pipeline.exec.started", "deps", &planValue, nil, nil, nil, "running dependency install plan")
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: execStartedMessage(i18n.Msg().ExecDepsLabel, execCommandDetails(planValue.DisplayCommand())),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	a.cancelAI = cancel
	return a, func() tea.Msg {
		defer cancel()
		result, err := proj.RunRestrictedPlan(ctx, planValue)
		return depsInstallMsg{result: result, err: err}
	}
}

func (a App) startTestsPhase() (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	a.progress.SetStepStatus(stepTests, StepRunning)
	a.progress.SetStepDetail(stepTests, m.ProgressRunningTests)

	if a.project == nil {
		return a.buildComplete()
	}

	plan, err := a.project.DetectTestPlan()
	if err != nil {
		emitExecTrace(a.router, "pipeline.exec.plan_error", "tests", nil, nil, nil, err, "failed to detect test command")
		a.progress.SetStepStatus(stepTests, StepFailed)
		a.progress.SetStepDetail(stepTests, err.Error())
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.BuildTestsDetectFailed, err),
		})
		return a.buildComplete()
	}
	a.pendingTestsPlan = plan

	if plan == nil {
		emitExecTrace(a.router, "pipeline.exec.not_detected", "tests", nil, nil, nil, nil, "no test command detected")
		a.progress.SetStepStatus(stepTests, StepDone)
		a.progress.SetStepDetail(stepTests, m.ProgressNoTests)
		return a.buildComplete()
	}
	emitExecTrace(a.router, "pipeline.exec.plan_detected", "tests", plan, nil, nil, nil, "detected test execution plan")

	if !a.pipeline.TestsApproved() && a.shouldAutoApproveRestrictedPlan(plan) {
		a.pipeline.SetTestsApproved(true)
		emitExecTrace(a.router, "pipeline.exec.confirmed", "tests", plan, boolPtr(true), nil, nil, "test execution auto-approved by safe mode")
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: m.ApprovalAutoTests + "\n" + plannedCommandDetails(plan.DisplayCommand()),
		})
		return a.runTestsPlan(plan)
	}

	if !a.pipeline.TestsApproved() {
		a.state = StateConfirmTests
		a.setPendingApproval(
			approvalTests,
			m.ApprovalTestsConfirm,
			plannedCommandDetails(plan.DisplayCommand()),
		)
		emitExecTrace(a.router, "pipeline.exec.confirm_requested", "tests", plan, nil, nil, nil, "waiting for test execution confirmation")
		if notice := a.restrictedPlanIsolationNotice(); notice != "" {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: notice,
			})
		}
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: approvalPrompt(m.ApprovalTestsConfirm, plannedCommandDetails(plan.DisplayCommand())),
		})
		return a, nil
	}

	return a.runTestsPlan(plan)
}

func (a App) runTestsPlan(plan *engine.ExecPlan) (tea.Model, tea.Cmd) {
	if a.project == nil || plan == nil {
		return a, nil
	}
	a.clearPendingApproval()
	// Fail closed when sandbox isolation is unavailable (see runDepsPlan).
	if notice := restrictedPlanBlockedNotice(); notice != "" {
		emitExecTrace(a.router, "pipeline.exec.skipped", "tests", plan, nil, nil, nil, "sandbox isolation unavailable")
		a.progress.SetStepStatus(stepTests, StepDone)
		a.progress.SetStepDetail(stepTests, testsRunSkippedDetail)
		a.chat.AddMessage(ChatMessage{Role: "system", Content: notice})
		a.pendingTestsPlan = nil
		return a.buildComplete()
	}
	proj := a.project
	planValue := *plan
	emitExecTrace(a.router, "pipeline.exec.started", "tests", &planValue, nil, nil, nil, "running test execution plan")
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: execStartedMessage(i18n.Msg().ExecTestsLabel, execCommandDetails(planValue.DisplayCommand())),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	a.cancelAI = cancel
	return a, func() tea.Msg {
		defer cancel()
		result, err := proj.RunRestrictedPlan(ctx, planValue)
		return testRunMsg{result: result, err: err}
	}
}

// --- Dependency installation ---

func (a App) handleDepsInstallConfirm(msg confirmDepsInstallMsg) (tea.Model, tea.Cmd) {
	a.clearPendingApproval()
	if !msg.confirmed {
		emitExecTrace(a.router, "pipeline.exec.declined", "deps", a.pendingDepsPlan, boolPtr(false), nil, nil, "dependency install declined by user")
		emitExecTrace(a.router, "pipeline.exec.skipped", "tests", a.pendingTestsPlan, nil, nil, nil, testsSkippedDetail)
		a.progress.SetStepStatus(stepDeps, StepDone)
		a.progress.SetStepDetail(stepDeps, depsInstallSkippedDetail)
		a.progress.SetStepStatus(stepTests, StepDone)
		a.progress.SetStepDetail(stepTests, testsSkippedDetail)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().BuildDepsSkipped,
		})
		a.pendingDepsPlan = nil
		a.pendingTestsPlan = nil
		a.pipeline.OnDepsDeclined()
		return a.buildComplete()
	}

	emitExecTrace(a.router, "pipeline.exec.confirmed", "deps", a.pendingDepsPlan, boolPtr(true), nil, nil, "dependency install approved by user")
	a.pipeline.SetDepsApproved(true)
	if a.pendingDepsPlan == nil {
		return a.startTestsPhase()
	}
	return a.runDepsPlan(a.pendingDepsPlan)
}

func (a App) handleDepsInstall(msg depsInstallMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	plan := a.pendingDepsPlan
	commandDetails := ""
	if plan != nil {
		commandDetails = execCommandDetails(plan.DisplayCommand())
	}

	switch {
	case msg.err != nil:
		emitExecTrace(a.router, "pipeline.exec.error", "deps", plan, nil, msg.result, msg.err, "dependency install execution failed")
		a.progress.SetStepStatus(stepDeps, StepFailed)
		a.progress.SetStepDetail(stepDeps, msg.err.Error())
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.BuildDepsExecError, msg.err),
		})
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage(m.ExecDepsLabel, commandDetails, execResultSummary(msg.result)),
		})
		// Still try tests
	case msg.result != nil && msg.result.ExitCode != 0:
		emitExecTrace(a.router, "pipeline.exec.failed", "deps", plan, nil, msg.result, nil, "dependency install command exited non-zero")
		a.progress.SetStepStatus(stepDeps, StepFailed)
		stderr := strings.TrimSpace(msg.result.Stderr)
		if stderr == "" {
			stderr = strings.TrimSpace(msg.result.Stdout)
		}
		a.progress.SetStepDetail(stepDeps, m.ErrBuildFail)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.BuildDepsExecFailed, stderr),
		})
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage(m.ExecDepsLabel, commandDetails, execResultSummary(msg.result)),
		})
		// Deps failure means the code was broken → quality penalty for code provider.
		if a.pipeline.CodeProvider() != "" {
			a.router.RecordQualityOutcome(model.PhaseCode, a.pipeline.CodeProvider(), false)
		}
		// Ask pipeline whether to auto-fix
		action := a.pipeline.OnDepsComplete(true, stderr)
		return a, func() tea.Msg {
			return autoFixMsg{errOutput: stderr, attempt: action.AutoFixAttempt}
		}
	case msg.result != nil && strings.Contains(msg.result.Stdout, "No package manager"):
		emitExecTrace(a.router, "pipeline.exec.not_detected", "deps", plan, nil, msg.result, nil, "no dependency install command detected")
		a.progress.SetStepStatus(stepDeps, StepDone)
		a.progress.SetStepDetail(stepDeps, m.ProgressNoDeps)
	default:
		emitExecTrace(a.router, "pipeline.exec.succeeded", "deps", plan, nil, msg.result, nil, "dependency install completed")
		a.progress.SetStepStatus(stepDeps, StepDone)
		a.progress.SetStepDetail(stepDeps, m.ProgressDepsInstalled)
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage(m.ExecDepsLabel, commandDetails, execResultSummary(msg.result)),
		})
	}
	a.pendingDepsPlan = nil

	// Next step: ask/execute tests
	return a.startTestsPhase()
}

func (a App) handleTestsRunConfirm(msg confirmTestsRunMsg) (tea.Model, tea.Cmd) {
	a.clearPendingApproval()
	if !msg.confirmed {
		emitExecTrace(a.router, "pipeline.exec.declined", "tests", a.pendingTestsPlan, boolPtr(false), nil, nil, "test execution declined by user")
		a.progress.SetStepStatus(stepTests, StepDone)
		a.progress.SetStepDetail(stepTests, testsRunSkippedDetail)
		a.pendingTestsPlan = nil
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().BuildTestsSkipped,
		})
		return a.buildComplete()
	}

	emitExecTrace(a.router, "pipeline.exec.confirmed", "tests", a.pendingTestsPlan, boolPtr(true), nil, nil, "test execution approved by user")
	a.pipeline.SetTestsApproved(true)
	if a.pendingTestsPlan == nil {
		return a.buildComplete()
	}
	return a.runTestsPlan(a.pendingTestsPlan)
}

// --- Test running ---

func (a App) handleTestRun(msg testRunMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	plan := a.pendingTestsPlan
	commandDetails := ""
	if plan != nil {
		commandDetails = execCommandDetails(plan.DisplayCommand())
	}
	if !msg.noTest && msg.result != nil && strings.Contains(msg.result.Stdout, "No test framework") {
		msg.noTest = true
	}

	if msg.noTest {
		emitExecTrace(a.router, "pipeline.exec.not_detected", "tests", plan, nil, msg.result, nil, "no tests detected")
		a.progress.SetStepStatus(stepTests, StepDone)
		a.progress.SetStepDetail(stepTests, m.ProgressNoTests)
		a.pendingTestsPlan = nil
		return a.buildComplete()
	}

	if msg.err != nil {
		emitExecTrace(a.router, "pipeline.exec.error", "tests", plan, nil, msg.result, msg.err, "test execution failed")
		a.progress.SetStepStatus(stepTests, StepFailed)
		a.progress.SetStepDetail(stepTests, msg.err.Error())
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage(m.ExecTestsLabel, commandDetails, execResultSummary(msg.result)),
		})
		a.pendingTestsPlan = nil
		return a.buildComplete()
	}

	if msg.result != nil && msg.result.ExitCode != 0 {
		emitExecTrace(a.router, "pipeline.exec.failed", "tests", plan, nil, msg.result, nil, "test command exited non-zero")
		a.progress.SetStepStatus(stepTests, StepFailed)
		a.progress.SetStepDetail(stepTests, m.ProgressTestsFailed)

		stderr := strings.TrimSpace(msg.result.Stderr)
		if stderr == "" {
			stderr = strings.TrimSpace(msg.result.Stdout)
		}
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("%s:\n%s", m.ErrTestFail, stderr),
		})
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage(m.ExecTestsLabel, commandDetails, execResultSummary(msg.result)),
		})
		// Test failure → code was broken; penalise the code provider.
		if a.pipeline.CodeProvider() != "" {
			a.router.RecordQualityOutcome(model.PhaseCode, a.pipeline.CodeProvider(), false)
		}

		// Ask pipeline whether to auto-fix
		action := a.pipeline.OnTestsComplete(true, stderr)
		return a, func() tea.Msg {
			return autoFixMsg{errOutput: stderr, attempt: action.AutoFixAttempt}
		}
	}

	emitExecTrace(a.router, "pipeline.exec.succeeded", "tests", plan, nil, msg.result, nil, "test execution completed")
	a.progress.SetStepStatus(stepTests, StepDone)
	a.progress.SetStepDetail(stepTests, m.ProgressTestsPassed)
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: execFinishedMessage(m.ExecTestsLabel, commandDetails, execResultSummary(msg.result)),
	})
	// Only reward the code provider if no auto-fix was needed.
	// If auto-fix ran, credit goes to the fix provider (below), not the original generator.
	if a.pipeline.ShouldRecordCodeQuality() {
		a.router.RecordQualityOutcome(model.PhaseCode, a.pipeline.CodeProvider(), true)
	}
	a.pipeline.OnTestsComplete(false, "")
	a.pendingTestsPlan = nil
	return a.buildComplete()
}

// --- Auto-fix loop ---

func (a App) handleAutoFix(msg autoFixMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	a = a.applyBudgetRoutingPolicy()

	action := a.pipeline.OnAutoFixAttempt(msg.attempt)
	if action.Kind == engine.ActionMaxRetriesReached {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.ErrMaxRetries, maxAutoFixRetries),
		})
		return a.buildComplete()
	}

	// Add auto-fix step to progress
	stepLabel := fmt.Sprintf(m.ProgressAutoFix, msg.attempt, maxAutoFixRetries)
	a.progress.AddStep(stepLabel, m.ErrAutofix)
	fixStepIdx := a.progress.TotalCount() - 1
	a.progress.SetStepStatus(fixStepIdx, StepRunning)

	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf("%s (%d/%d)", m.ErrAutofix, msg.attempt, maxAutoFixRetries),
	})
	if a.shouldUseAutopilotCandidates() {
		a.activity.Start()
		a.activity.SetPhase(chatActivityWaiting, "", "", false, m.AutomationCandidateStarted)
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: m.AutomationCandidateStarted,
		})
	}

	proj := a.project
	router := a.router
	errOutput := msg.errOutput
	attempt := msg.attempt
	codeProvider := a.pipeline.CodeProvider()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	a.cancelAI = cancel
	return a, func() tea.Msg {
		defer cancel()

		messages := []model.Message{{Role: "user", Content: buildAutoFixUserPrompt(errOutput)}}
		systemPrompt := buildAutoFixSystemPrompt(proj)

		if a.shouldUseAutopilotCandidates() {
			selection := runCandidateSelectionWithActivity(ctx, a.activity, router, proj, model.PhaseFix, messages, systemPrompt, codeProvider)
			if strings.TrimSpace(selection.content) == "" {
				return autoFixResponseMsg{
					provider:      selection.provider,
					cost:          selection.usage.Cost,
					inputTokens:   selection.usage.InputTokens,
					outputTokens:  selection.usage.OutputTokens,
					attempt:       attempt,
					selectionNote: selection.selectionNote,
					err:           selection.contentError(fmt.Errorf("no candidate provider produced writable fixes")),
				}
			}
			return autoFixResponseMsg{
				content:       selection.content,
				provider:      selection.provider,
				cost:          selection.usage.Cost,
				inputTokens:   selection.usage.InputTokens,
				outputTokens:  selection.usage.OutputTokens,
				attempt:       attempt,
				verified:      selection.verified,
				selectionNote: selection.selectionNote,
			}
		}

		// ChatBest: Power mode uses ensemble+judge; others use the primary fix provider.
		// Exclude the code provider so the fix is always cross-model.
		content, usage, route, err := router.ChatBest(ctx, model.PhaseFix, messages, systemPrompt, codeProvider)
		provider := providerForUsage(usage, route)
		if err != nil {
			return autoFixResponseMsg{
				provider:     provider,
				cost:         usage.Cost,
				inputTokens:  usage.InputTokens,
				outputTokens: usage.OutputTokens,
				attempt:      attempt,
				err:          err,
			}
		}

		return autoFixResponseMsg{
			content:      content,
			provider:     provider,
			cost:         usage.Cost,
			inputTokens:  usage.InputTokens,
			outputTokens: usage.OutputTokens,
			attempt:      attempt,
		}
	}
}

func (a App) handleAutoFixResponse(msg autoFixResponseMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	fixStepIdx := a.progress.TotalCount() - 1

	// Surface the selection note (which carries delete-only warnings) before the
	// error branch so a delete-only fix candidate that produced no writable
	// content still shows its deletion warning to the user.
	if msg.selectionNote != "" {
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: msg.selectionNote,
		})
	}

	if msg.err != nil {
		a.recordKnownUsage(msg.provider, msg.cost, msg.inputTokens, msg.outputTokens)
		a.progress.SetStepStatus(fixStepIdx, StepFailed)
		a.activity.Reset()
		// A fail-closed untrusted-mode sentinel gets the actionable message; other
		// errors keep the "Auto-fix error:" prefix for context.
		content := fmt.Sprintf("Auto-fix error: %s", msg.err)
		if errors.Is(msg.err, model.ErrNoUntrustedSafeProvider) {
			content = chatErrorContent(msg.err)
		}
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: content,
		})
		return a.buildComplete()
	}

	// Track cost
	isSub := a.router.IsSubscription(msg.provider)
	a.cost.AddWithTokens(msg.provider, msg.cost, msg.inputTokens, msg.outputTokens, isSub)

	a.chat.AddMessage(ChatMessage{
		Role:     "assistant",
		Content:  msg.content,
		Provider: msg.provider,
		Cost:     msg.cost,
	})
	a.activity.Reset()

	// Parse fix files
	result := engine.ParseFilesBestEffort(msg.content)
	pipelineAction := a.pipeline.OnAutoFixResponse(len(result.Files) > 0, msg.attempt)
	if pipelineAction.Kind == engine.ActionMaxRetriesReached {
		a.progress.SetStepStatus(fixStepIdx, StepFailed)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.ErrMaxRetries, maxAutoFixRetries),
		})
		return a.buildComplete()
	}

	a.progress.SetStepStatus(fixStepIdx, StepDone)

	// Reward the fix provider: it successfully repaired broken code.
	if msg.provider != "" {
		a.router.RecordQualityOutcome(model.PhaseFix, msg.provider, true)
	}

	// Ask for confirmation before writing fix files
	a.pendingWriteVerified = msg.verified
	return a, func() tea.Msg {
		return filesExtractedMsg{files: result.Files, phase: pendingPhaseFix}
	}
}

// handleAutoFixFileWriteComplete continues the auto-fix retry after files have been written.
func (a App) handleAutoFixFileWriteComplete() (tea.Model, tea.Cmd) {
	a.pipeline.OnAutoFixFilesWritten()
	proj := a.project
	router := a.router
	attempt := a.pipeline.AutoFixRetryAttempt()

	// A deps/tests approval is single-use: it grants ONE execution. It must not be
	// silently replayed to run generated commands on the host again on a later
	// retry. Clear any earlier grant so this retry is re-gated from scratch.
	a.pipeline.SetDepsApproved(false)
	a.pipeline.SetTestsApproved(false)

	// A retry re-runs deps/tests to verify the fix. When those restricted commands
	// cannot run because sandbox isolation is unavailable (and no
	// MAKEWAND_UNSAFE_HOST_EXEC opt-in), do NOT execute generated commands on the
	// host. Surface the notice and stop the build instead of looping through
	// fruitless auto-fix attempts.
	if notice := restrictedPlanBlockedNotice(); notice != "" {
		emitExecTrace(router, "pipeline.exec.skipped", "tests", nil, nil, nil, nil, "sandbox isolation unavailable; auto-fix retry left unverified")
		a.chat.AddMessage(ChatMessage{Role: "system", Content: notice})
		return a.buildComplete()
	}

	// Manual approval mode never auto-runs restricted plans: the retry must
	// re-prompt through the normal deps/tests gate rather than replaying the
	// earlier one-time approval. startDepsPhase re-asks (or, when there is no deps
	// plan, flows on to the tests gate). This is user-gated, so it cannot loop.
	if !a.safeApprovalEnabled() {
		return a.startDepsPhase()
	}

	// Safe/autopilot mode with isolation available: re-run deps + tests
	// automatically as one retry step. The decision to run is re-derived from the
	// current isolation state above (not a replayed approval token), and the
	// incrementing auto-fix attempt is tracked so max-retries still terminates.
	return a, func() tea.Msg {
		ctx := context.Background()

		depsPlan, planErr := proj.DetectInstallPlan()
		if planErr != nil {
			emitExecTrace(router, "pipeline.exec.plan_error", "deps", nil, nil, nil, planErr, "auto-fix retry failed to detect dependency install command")
			return autoFixMsg{errOutput: planErr.Error(), attempt: attempt + 1}
		}
		if depsPlan != nil {
			emitExecTrace(router, "pipeline.exec.plan_detected", "deps", depsPlan, nil, nil, nil, "auto-fix retry detected dependency install plan")
			emitExecTrace(router, "pipeline.exec.started", "deps", depsPlan, nil, nil, nil, "auto-fix retry running dependency install plan")
			depsResult, err := proj.RunRestrictedPlan(ctx, *depsPlan)
			if err != nil || (depsResult != nil && depsResult.ExitCode != 0) {
				if err != nil {
					emitExecTrace(router, "pipeline.exec.error", "deps", depsPlan, nil, depsResult, err, "auto-fix retry dependency install failed")
				} else {
					emitExecTrace(router, "pipeline.exec.failed", "deps", depsPlan, nil, depsResult, nil, "auto-fix retry dependency install exited non-zero")
				}
				errOut := ""
				if depsResult != nil {
					errOut = depsResult.Stderr + depsResult.Stdout
				} else if err != nil {
					errOut = err.Error()
				}
				return autoFixMsg{errOutput: errOut, attempt: attempt + 1}
			}
			emitExecTrace(router, "pipeline.exec.succeeded", "deps", depsPlan, nil, depsResult, nil, "auto-fix retry dependency install completed")
		} else {
			emitExecTrace(router, "pipeline.exec.not_detected", "deps", nil, nil, nil, nil, "auto-fix retry found no dependency install command")
		}

		// Then tests
		testsPlan, planErr := proj.DetectTestPlan()
		if planErr != nil {
			emitExecTrace(router, "pipeline.exec.plan_error", "tests", nil, nil, nil, planErr, "auto-fix retry failed to detect test command")
			return autoFixMsg{errOutput: planErr.Error(), attempt: attempt + 1}
		}
		if testsPlan == nil {
			emitExecTrace(router, "pipeline.exec.not_detected", "tests", nil, nil, nil, nil, "auto-fix retry found no test command")
			return testRunMsg{noTest: true}
		}

		emitExecTrace(router, "pipeline.exec.plan_detected", "tests", testsPlan, nil, nil, nil, "auto-fix retry detected test execution plan")
		emitExecTrace(router, "pipeline.exec.started", "tests", testsPlan, nil, nil, nil, "auto-fix retry running test execution plan")
		testResult, err := proj.RunRestrictedPlan(ctx, *testsPlan)
		if err != nil || (testResult != nil && testResult.ExitCode != 0) {
			if err != nil {
				emitExecTrace(router, "pipeline.exec.error", "tests", testsPlan, nil, testResult, err, "auto-fix retry test execution failed")
			} else {
				emitExecTrace(router, "pipeline.exec.failed", "tests", testsPlan, nil, testResult, nil, "auto-fix retry test command exited non-zero")
			}
			if testResult != nil && strings.Contains(testResult.Stdout, "No test framework") {
				emitExecTrace(router, "pipeline.exec.not_detected", "tests", testsPlan, nil, testResult, nil, "auto-fix retry reported no test framework")
				return testRunMsg{result: testResult, noTest: true}
			}
			errOut := ""
			if testResult != nil {
				errOut = testResult.Stderr + testResult.Stdout
			} else if err != nil {
				errOut = err.Error()
			}
			return autoFixMsg{errOutput: errOut, attempt: attempt + 1}
		}
		emitExecTrace(router, "pipeline.exec.succeeded", "tests", testsPlan, nil, testResult, nil, "auto-fix retry test execution completed")

		return testRunMsg{result: testResult, noTest: false}
	}
}
