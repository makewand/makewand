package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

const depsInstallConfirmPrompt = "Install dependencies now? This may execute scripts from generated project files. (Y/n)"
const depsInstallSkippedDetail = "skipped for safety"
const testsSkippedDetail = "skipped (dependency install not approved)"
const testsRunSkippedDetail = "skipped by user"

// --- AI response handling ---

func (a App) handleAIResponse(msg aiResponseMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Error: %s", msg.err),
		})
		a.chat.SetStreaming(false)
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
	a.chat.SetStreaming(false)

	// Record the code provider for cross-model review
	if a.wizard.Phase() == WizardPhaseBuild && a.buildCodeProvider == "" {
		a.buildCodeProvider = msg.provider
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
			return a, func() tea.Msg {
				return filesExtractedMsg{files: result.Files, phase: phase}
			}
		}
	}

	return a, nil
}

// --- File extraction and writing ---

func (a App) handleFilesExtracted(msg filesExtractedMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	a.pendingFiles = msg.files
	a.pendingPhase = msg.phase

	if msg.phase == pendingPhaseBuild {
		// Build phase: auto-confirm file writing
		a.progress.SetStepDetail(stepCode, fmt.Sprintf(m.ProgressFilesFound, len(msg.files)))
		return a, func() tea.Msg {
			return confirmFileWriteMsg{confirmed: true}
		}
	}

	// Review, fix, and chat phases: ask for confirmation
	a.confirmingFiles = true
	a.setPendingApproval(
		approvalFileWrite,
		fmt.Sprintf(m.FileConfirmWrite, len(msg.files)),
		fmt.Sprintf("Pending write: %d files", len(msg.files)),
	)
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: approvalPrompt(fmt.Sprintf(m.FileConfirmWrite, len(msg.files)), fmt.Sprintf("Pending write: %d files", len(msg.files))),
	})
	return a, nil
}

func (a App) handleFileConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	key := strings.ToLower(msg.String())

	switch key {
	case "y", "enter":
		a.confirmingFiles = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmFileWriteMsg{confirmed: true}
		}
	case "n", "esc":
		a.confirmingFiles = false
		a.clearPendingApproval()
		a.pendingFiles = nil
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
		a.confirmingDeps = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmDepsInstallMsg{confirmed: true}
		}
	case "n", "esc":
		a.confirmingDeps = false
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
		a.confirmingTests = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmTestsRunMsg{confirmed: true}
		}
	case "n", "esc":
		a.confirmingTests = false
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

	if a.pendingPhase == pendingPhaseReview {
		// Review fix files written — continue to deps phase
		a.progress.SetStepStatus(stepReview, StepDone)
		a.progress.SetStepDetail(stepReview, fmt.Sprintf(m.ProgressReviewApplied, msg.written, a.buildReviewProvider))
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

		// Next step: code review (cross-model)
		// Skip review if only one provider is available — reviewing your own code is pointless
		codeProvider := a.buildCodeProvider
		availableProviders := a.router.Available()
		if len(availableProviders) <= 1 {
			a.progress.SetStepStatus(stepReview, StepDone)
			a.progress.SetStepDetail(stepReview, "skipped (single provider)")
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
			a.buildReviewProvider = reviewProvider
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
						fileContents.WriteString(fmt.Sprintf("(... %d more files omitted for size)\n", len(proj.Files)))
						break
					}
					fileContents.WriteString(entry)
				}
			}

			reviewPrompt := buildReviewUserPrompt(fileContents.String())
			messages := []model.Message{{Role: "user", Content: reviewPrompt}}

			// ChatBest: Power mode uses ensemble+judge; others use the single review provider.
			// Exclude the code provider so cross-model constraint is always enforced.
			content, usage, _, err := router.ChatBest(ctx, model.PhaseReview, messages, codeReviewSystemPrompt, codeProvider)
			if err != nil {
				return codeReviewMsg{err: err, provider: codeProvider}
			}

			return codeReviewMsg{
				content:      content,
				provider:     usage.Provider,
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
		// Review error is non-fatal — skip review and continue to deps
		a.progress.SetStepStatus(stepReview, StepDone)
		a.progress.SetStepDetail(stepReview, fmt.Sprintf("skipped: %s", msg.err))
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Review skipped: %s", msg.err),
		})
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
		return a.startDepsPhase()
	}

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
			Content: fmt.Sprintf("Dependency detection failed: %s", err),
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

	if !a.depsInstallApproved {
		a.confirmingDeps = true
		a.setPendingApproval(
			approvalDeps,
			depsInstallConfirmPrompt,
			fmt.Sprintf("Planned command: %s", plan.DisplayCommand()),
		)
		emitExecTrace(a.router, "pipeline.exec.confirm_requested", "deps", plan, nil, nil, nil, "waiting for dependency install confirmation")
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: approvalPrompt(depsInstallConfirmPrompt, fmt.Sprintf("Planned command: %s", plan.DisplayCommand())),
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
	proj := a.project
	planValue := *plan
	emitExecTrace(a.router, "pipeline.exec.started", "deps", &planValue, nil, nil, nil, "running dependency install plan")
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: execStartedMessage("dependency install", fmt.Sprintf("Command: %s", planValue.DisplayCommand())),
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
			Content: fmt.Sprintf("Test detection failed: %s", err),
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

	if !a.testsRunApproved {
		a.confirmingTests = true
		a.setPendingApproval(
			approvalTests,
			"Run project tests now?",
			fmt.Sprintf("Planned command: %s", plan.DisplayCommand()),
		)
		emitExecTrace(a.router, "pipeline.exec.confirm_requested", "tests", plan, nil, nil, nil, "waiting for test execution confirmation")
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: approvalPrompt("Run project tests now?", fmt.Sprintf("Planned command: %s", plan.DisplayCommand())),
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
	proj := a.project
	planValue := *plan
	emitExecTrace(a.router, "pipeline.exec.started", "tests", &planValue, nil, nil, nil, "running test execution plan")
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: execStartedMessage("tests", fmt.Sprintf("Command: %s", planValue.DisplayCommand())),
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
			Content: "Skipped dependency install and tests. Run them manually when you're ready.",
		})
		a.pendingDepsPlan = nil
		a.pendingTestsPlan = nil
		return a.buildComplete()
	}

	emitExecTrace(a.router, "pipeline.exec.confirmed", "deps", a.pendingDepsPlan, boolPtr(true), nil, nil, "dependency install approved by user")
	a.depsInstallApproved = true
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
		commandDetails = fmt.Sprintf("Command: %s", plan.DisplayCommand())
	}

	if msg.err != nil {
		emitExecTrace(a.router, "pipeline.exec.error", "deps", plan, nil, msg.result, msg.err, "dependency install execution failed")
		a.progress.SetStepStatus(stepDeps, StepFailed)
		a.progress.SetStepDetail(stepDeps, msg.err.Error())
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Error installing dependencies: %s", msg.err),
		})
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage("dependency install", commandDetails, execResultSummary(msg.result)),
		})
		// Still try tests
	} else if msg.result != nil && msg.result.ExitCode != 0 {
		emitExecTrace(a.router, "pipeline.exec.failed", "deps", plan, nil, msg.result, nil, "dependency install command exited non-zero")
		a.progress.SetStepStatus(stepDeps, StepFailed)
		stderr := strings.TrimSpace(msg.result.Stderr)
		if stderr == "" {
			stderr = strings.TrimSpace(msg.result.Stdout)
		}
		a.progress.SetStepDetail(stepDeps, m.ErrBuildFail)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Dependency install failed:\n%s", stderr),
		})
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage("dependency install", commandDetails, execResultSummary(msg.result)),
		})
		// Deps failure means the code was broken → quality penalty for code provider.
		if a.buildCodeProvider != "" {
			a.router.RecordQualityOutcome(model.PhaseCode, a.buildCodeProvider, false)
		}
		// Trigger auto-fix for dependency issues
		return a, func() tea.Msg {
			return autoFixMsg{errOutput: stderr, attempt: 1}
		}
	} else if msg.result != nil && strings.Contains(msg.result.Stdout, "No package manager") {
		emitExecTrace(a.router, "pipeline.exec.not_detected", "deps", plan, nil, msg.result, nil, "no dependency install command detected")
		a.progress.SetStepStatus(stepDeps, StepDone)
		a.progress.SetStepDetail(stepDeps, m.ProgressNoDeps)
	} else {
		emitExecTrace(a.router, "pipeline.exec.succeeded", "deps", plan, nil, msg.result, nil, "dependency install completed")
		a.progress.SetStepStatus(stepDeps, StepDone)
		a.progress.SetStepDetail(stepDeps, m.ProgressDepsInstalled)
		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: execFinishedMessage("dependency install", commandDetails, execResultSummary(msg.result)),
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
			Content: "Skipped tests. Run them manually when you're ready.",
		})
		return a.buildComplete()
	}

	emitExecTrace(a.router, "pipeline.exec.confirmed", "tests", a.pendingTestsPlan, boolPtr(true), nil, nil, "test execution approved by user")
	a.testsRunApproved = true
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
		commandDetails = fmt.Sprintf("Command: %s", plan.DisplayCommand())
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
			Content: execFinishedMessage("tests", commandDetails, execResultSummary(msg.result)),
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
			Content: execFinishedMessage("tests", commandDetails, execResultSummary(msg.result)),
		})
		// Test failure → code was broken; penalise the code provider.
		if a.buildCodeProvider != "" {
			a.router.RecordQualityOutcome(model.PhaseCode, a.buildCodeProvider, false)
		}

		// Trigger auto-fix
		return a, func() tea.Msg {
			return autoFixMsg{errOutput: stderr, attempt: 1}
		}
	}

	emitExecTrace(a.router, "pipeline.exec.succeeded", "tests", plan, nil, msg.result, nil, "test execution completed")
	a.progress.SetStepStatus(stepTests, StepDone)
	a.progress.SetStepDetail(stepTests, m.ProgressTestsPassed)
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: execFinishedMessage("tests", commandDetails, execResultSummary(msg.result)),
	})
	// Only reward the code provider if no auto-fix was needed.
	// If auto-fix ran, credit goes to the fix provider (below), not the original generator.
	if a.buildCodeProvider != "" && a.autoFixAttempt == 0 {
		a.router.RecordQualityOutcome(model.PhaseCode, a.buildCodeProvider, true)
	}
	a.pendingTestsPlan = nil
	return a.buildComplete()
}

// --- Auto-fix loop ---

func (a App) handleAutoFix(msg autoFixMsg) (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	a = a.applyBudgetRoutingPolicy()

	if msg.attempt > maxAutoFixRetries {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(m.ErrMaxRetries, maxAutoFixRetries),
		})
		return a.buildComplete()
	}

	a.autoFixAttempt = msg.attempt

	// Add auto-fix step to progress
	stepLabel := fmt.Sprintf(m.ProgressAutoFix, msg.attempt, maxAutoFixRetries)
	a.progress.AddStep(stepLabel, m.ErrAutofix)
	fixStepIdx := a.progress.TotalCount() - 1
	a.progress.SetStepStatus(fixStepIdx, StepRunning)

	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf("%s (%d/%d)", m.ErrAutofix, msg.attempt, maxAutoFixRetries),
	})

	proj := a.project
	router := a.router
	errOutput := msg.errOutput
	attempt := msg.attempt
	codeProvider := a.buildCodeProvider

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	a.cancelAI = cancel
	return a, func() tea.Msg {
		defer cancel()

		messages := []model.Message{{Role: "user", Content: buildAutoFixUserPrompt(errOutput)}}
		systemPrompt := buildAutoFixSystemPrompt(proj)

		// ChatBest: Power mode uses ensemble+judge; others use the primary fix provider.
		// Exclude the code provider so the fix is always cross-model.
		content, usage, _, err := router.ChatBest(ctx, model.PhaseFix, messages, systemPrompt, codeProvider)
		if err != nil {
			return autoFixResponseMsg{err: err, attempt: attempt}
		}

		return autoFixResponseMsg{
			content:      content,
			provider:     usage.Provider,
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

	if msg.err != nil {
		a.progress.SetStepStatus(fixStepIdx, StepFailed)
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Auto-fix error: %s", msg.err),
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

	// Parse fix files
	result := engine.ParseFilesBestEffort(msg.content)
	if len(result.Files) == 0 {
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

	// Save retry state for after file write confirmation
	a.autoFixRetryAttempt = msg.attempt

	// Ask for confirmation before writing fix files
	return a, func() tea.Msg {
		return filesExtractedMsg{files: result.Files, phase: pendingPhaseFix}
	}
}

// handleAutoFixFileWriteComplete continues the auto-fix retry after files have been written.
func (a App) handleAutoFixFileWriteComplete() (tea.Model, tea.Cmd) {
	proj := a.project
	router := a.router
	attempt := a.autoFixRetryAttempt
	depsApproved := a.depsInstallApproved
	testsApproved := a.testsRunApproved
	var cmds []tea.Cmd

	// Retry: re-run deps + tests
	cmds = append(cmds, func() tea.Msg {
		ctx := context.Background()
		// Re-run dependency install only when explicitly approved.
		if depsApproved {
			depsPlan, planErr := proj.DetectInstallPlan()
			if planErr != nil {
				emitExecTrace(router, "pipeline.exec.plan_error", "deps", nil, nil, nil, planErr, "auto-fix retry failed to detect dependency install command")
				errOut := ""
				if planErr != nil {
					errOut = planErr.Error()
				}
				return autoFixMsg{errOutput: errOut, attempt: attempt + 1}
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
		}

		if !testsApproved {
			return resumeTestsPhaseMsg{}
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
	})

	return a, tea.Batch(cmds...)
}
