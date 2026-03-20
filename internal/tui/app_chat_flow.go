package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

const chatStreamTimeout = 5 * time.Minute

func (a App) handleChatEnter() (tea.Model, tea.Cmd) {
	if a.streamCh != nil || a.chat.streaming {
		return a, nil
	}

	if a.chat.HasSlashSuggestions() && a.chat.ApplySelectedSlashSuggestion() {
		return a, nil
	}

	input := strings.TrimSpace(a.chat.InputValue())
	if input == "" {
		return a, nil
	}
	a.chat.ClearInput()
	return a.submitChatInput(input)
}

func (a App) submitChatInput(input string) (tea.Model, tea.Cmd) {
	if a.streamCh != nil || a.chat.streaming {
		return a, nil
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return a, nil
	}

	parts := strings.Fields(strings.ToLower(input))
	if len(parts) > 0 {
		// Handle slash commands locally (don't send to AI).
		switch parts[0] {
		case "/mode", "/model":
			return a.handleModeCommand(input)
		case "/approval":
			return a.handleApprovalModeCommand(input)
		case "/help":
			return a.handleHelpCommand()
		case "/clear":
			return a.handleClearCommand()
		case "/compact":
			return a.handleCompactCommand()
		case "/memory":
			return a.handleMemoryCommand()
		case "/status", "/doctor":
			return a.handleStatusCommand()
		case "/cost":
			return a.handleCostCommand()
		case "/approve":
			return a.handleApproveCommand()
		case "/deny":
			return a.handleDenyCommand()
		case "/resume":
			return a.handleResumeCommand()
		case "/exit", "/quit":
			if a.cancelAI != nil {
				a.cancelAI()
				a.cancelAI = nil
			}
			a.activity.Reset()
			a.state = StateQuitting
			return a, tea.Quit
		}
	}

	a.chat.AddMessage(ChatMessage{Role: "user", Content: input})
	a.chat.SetStreaming(true)
	a.state = StateStreaming
	a = a.applyBudgetRoutingPolicy()
	a.activity.Start()
	a.syncChatActivity()

	messages := a.chat.ToModelMessages()
	task := classifyTask(input)

	ctx, cancel := context.WithTimeout(context.Background(), chatStreamTimeout)
	a.cancelAI = cancel

	if a.shouldUseAutopilotChatCandidates(task) {
		project := a.project
		router := a.router
		activity := a.activity
		phase := chatTaskToBuildPhase(task)
		cmd := func() tea.Msg {
			defer cancel()
			markChatPreparationActivity(activity, project != nil)
			systemPrompt := buildSystemPrompt(project, task, router.Mode())
			activity.SetPhase(chatActivityWaiting, "", "", false, i18n.Msg().AutomationCandidateStarted)
			selection := runCandidateSelectionWithActivity(ctx, activity, router, project, phase, messages, systemPrompt)
			if strings.TrimSpace(selection.content) == "" {
				return aiResponseMsg{err: fmt.Errorf("no candidate provider produced a response")}
			}
			return aiResponseMsg{
				content:       selection.content,
				provider:      selection.provider,
				cost:          selection.usage.Cost,
				inputTokens:   selection.usage.InputTokens,
				outputTokens:  selection.usage.OutputTokens,
				verified:      selection.verified,
				selectionNote: selection.selectionNote,
			}
		}
		return a, cmd
	}

	// In power mode, chat uses multi-model ensemble + judge (ChatBest).
	// Other modes keep low-latency stream-first behavior.
	if a.router.ModeSet() && a.router.Mode() == model.ModePower {
		project := a.project
		router := a.router
		activity := a.activity
		phase := chatTaskToBuildPhase(task)
		cmd := func() tea.Msg {
			defer cancel()
			markChatPreparationActivity(activity, project != nil)
			systemPrompt := buildSystemPrompt(project, task, router.Mode())
			activity.SetPhase(chatActivityWaiting, "", "", false, i18n.Msg().ChatActivityMultiModel)
			content, usage, result, err := router.ChatBest(ctx, phase, messages, systemPrompt)
			if err != nil {
				return aiResponseMsg{err: err}
			}
			provider := result.Actual
			if provider == "" {
				provider = usage.Provider
			}
			return aiResponseMsg{
				content:      content,
				provider:     provider,
				cost:         usage.Cost,
				inputTokens:  usage.InputTokens,
				outputTokens: usage.OutputTokens,
			}
		}
		return a, cmd
	}

	// Explain/analyze prompts usually use unary chat for lower overhead.
	// Gemini CLI is a special case: it supports stream-json in headless mode,
	// which provides visible progress for repo-evaluation prompts.
	if shouldUseUnaryChat(task) && !a.shouldPreferStreamingChat(task) {
		project := a.project
		router := a.router
		activity := a.activity
		cmd := func() tea.Msg {
			defer cancel()
			markChatPreparationActivity(activity, project != nil)
			systemPrompt := buildSystemPrompt(project, task, router.Mode())
			route, routeErr := router.Route(task)
			markChatRoutingActivity(activity, route, routeErr)
			content, usage, result, err := router.Chat(ctx, task, messages, systemPrompt)
			if err != nil {
				return aiResponseMsg{err: err}
			}
			provider := result.Actual
			if provider == "" {
				provider = usage.Provider
			}
			return aiResponseMsg{
				content:      content,
				provider:     provider,
				cost:         usage.Cost,
				inputTokens:  usage.InputTokens,
				outputTokens: usage.OutputTokens,
			}
		}
		return a, cmd
	}

	project := a.project
	router := a.router
	activity := a.activity
	cmd := func() tea.Msg {
		markChatPreparationActivity(activity, project != nil)
		systemPrompt := buildSystemPrompt(project, task, router.Mode())
		route, routeErr := router.Route(task)
		markChatRoutingActivity(activity, route, routeErr)
		ch, result, err := router.ChatStream(ctx, task, messages, systemPrompt)
		if err != nil {
			return aiResponseMsg{err: err}
		}

		return aiStreamStartMsg{
			ch:         ch,
			provider:   result.Actual,
			isFallback: result.IsFallback,
			requested:  result.Requested,
		}
	}

	return a, cmd
}

func (a App) shouldPreferStreamingChat(task model.TaskType) bool {
	route, err := a.router.Route(task)
	if err != nil {
		return false
	}
	if route.Actual != "gemini" {
		return false
	}
	_, ok := route.Provider.(*model.CLIProvider)
	return ok
}

func (a App) shouldUseAutopilotChatCandidates(task model.TaskType) bool {
	if !a.shouldUseAutopilotCandidates() || a.project == nil {
		return false
	}
	switch task {
	case model.TaskCode, model.TaskFix:
		return true
	default:
		return false
	}
}

func markChatPreparationActivity(activity *chatActivityState, hasProject bool) {
	activity.SetPhase(chatActivityPreparing, "", "", false, "")
	if hasProject {
		activity.SetPhase(chatActivityContext, "", "", false, "")
	}
}

func markChatRoutingActivity(activity *chatActivityState, route model.RouteResult, routeErr error) {
	activity.SetPhase(chatActivityRouting, "", "", false, "")
	if routeErr == nil {
		activity.SetPhase(chatActivityWaiting, route.Actual, route.Requested, route.IsFallback, "")
		return
	}
	activity.SetPhase(chatActivityWaiting, "", "", false, "")
}

func (a App) handleHelpCommand() (tea.Model, tea.Cmd) {
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: a.chatHelpText(),
	})
	return a, nil
}

func (a App) handleClearCommand() (tea.Model, tea.Cmd) {
	a.chat.ResetMessages([]ChatMessage{a.chatWelcomeMessage()})
	a.cost = NewCostTracker()
	a.pendingFiles = nil
	a.pendingDepsPlan = nil
	a.pendingTestsPlan = nil
	a.state = StateIdle
	a.clearPendingApproval()
	a.activity.Reset()
	a.restoredSession = false
	a.restoredMessageCount = 0
	return a, tea.ClearScreen
}

func (a App) handleCompactCommand() (tea.Model, tea.Cmd) {
	if a.chat.CompactHistory() {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: "Conversation compacted.",
		})
		return a, nil
	}
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: "Nothing to compact yet.",
	})
	return a, nil
}

func (a App) handleMemoryCommand() (tea.Model, tea.Cmd) {
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: a.memorySummary(),
	})
	return a, nil
}

func (a App) handleStatusCommand() (tea.Model, tea.Cmd) {
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: a.statusSummary(),
	})
	return a, nil
}

func (a App) handleCostCommand() (tea.Model, tea.Cmd) {
	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: a.costSummary(),
	})
	return a, nil
}

func (a App) handleResumeCommand() (tea.Model, tea.Cmd) {
	restored, err := a.restoreChatSession()
	if err != nil {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Could not restore session: %s", err),
		})
		return a, nil
	}
	if !restored {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: "No saved session found for this project.",
		})
		return a, nil
	}
	return a, nil
}

func (a App) handleModeCommand(input string) (tea.Model, tea.Cmd) {
	msg := i18n.Msg()
	parts := strings.Fields(input)

	if len(parts) == 1 {
		// /mode with no argument: show current mode and help
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("%s: %s\n/model [fast|balanced|power]", msg.ModeLabel, a.currentModeLabel()),
		})
		return a, nil
	}

	// /mode <name>: set mode
	modeName := strings.ToLower(parts[1])
	mode, ok := model.ParseUsageMode(modeName)
	if !ok {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: msg.ModeHelp,
		})
		return a, nil
	}

	a.router.SetMode(mode)

	// Translate mode name for display
	var displayName string
	switch mode {
	case model.ModeFast:
		displayName = msg.ModeFast
	case model.ModeBalanced:
		displayName = msg.ModeBalanced
	case model.ModePower:
		displayName = msg.ModePower
	}

	a.chat.AddMessage(ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf("Model profile: %s", displayName),
	})
	return a, nil
}

func (a App) handleApprovalModeCommand(input string) (tea.Model, tea.Cmd) {
	msg := i18n.Msg()
	parts := strings.Fields(input)

	if len(parts) == 1 {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("%s: %s\n%s", msg.ApprovalModeLabel, a.currentApprovalModeLabel(), msg.ApprovalModeHelp),
		})
		return a, nil
	}

	switch strings.ToLower(parts[1]) {
	case "manual", "safe", "autopilot":
		a.setApprovalMode(parts[1])
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(msg.ApprovalModeChanged, a.currentApprovalModeLabel()),
		})
	default:
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: msg.ApprovalModeHelp,
		})
	}
	return a, nil
}

func (a App) currentModeLabel() string {
	if a.router.ModeSet() {
		return a.router.Mode().String()
	}
	return "not set (legacy routing)"
}

func (a App) chatHelpText() string {
	return strings.Join([]string{
		"Available commands:",
		"/help - Show command help",
		"/clear - Clear the current conversation",
		"/compact - Compact older chat history",
		"/memory - Show compacted session memory",
		"/status - Show current session status",
		"/cost - Show current session cost",
		"/approval [manual|safe|autopilot] - Switch approval behavior",
		"/approve - Approve the pending action",
		"/deny - Deny the pending action",
		"/resume - Restore the last saved session",
		"/model [fast|balanced|power] - Switch routing profile",
		"/exit - Quit makewand",
	}, "\n")
}

func (a App) statusSummary() string {
	var lines []string
	msg := i18n.Msg()
	lines = append(lines, fmt.Sprintf("Model profile: %s", a.currentModeLabel()))
	lines = append(lines, fmt.Sprintf("%s: %s", msg.ApprovalModeLabel, a.currentApprovalModeLabel()))

	available := a.router.Available()
	if len(available) == 0 {
		lines = append(lines, "Available providers: none")
	} else {
		lines = append(lines, "Available providers: "+strings.Join(available, ", "))
	}

	if a.project != nil {
		lines = append(lines, fmt.Sprintf("Project: %s", a.project.Path))
		lines = append(lines, fmt.Sprintf("Project entries: %d", len(a.project.Files)))
	}

	if summary := a.pendingApprovalSummary(); summary != "" {
		lines = append(lines, msg.ApprovalPendingLabel+":")
		lines = append(lines, summary)
	}
	if a.activity != nil {
		if snapshot := a.activity.Snapshot(); snapshot.Active {
			lines = append(lines, fmt.Sprintf("%s: %s", msg.ActivityLabel, formatChatActivityStatus(snapshot)))
		}
	}

	if a.sessionFile != "" {
		lines = append(lines, fmt.Sprintf("Session file: %s", a.sessionFile))
	}
	if a.lastSessionSavedAt != "" {
		lines = append(lines, fmt.Sprintf("Last saved: %s", sessionTimeLabel(a.lastSessionSavedAt)))
	}
	lines = append(lines, fmt.Sprintf("Session cost: $%.2f", a.cost.SessionTotal()))
	return strings.Join(lines, "\n")
}

func (a App) costSummary() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Session total: $%.2f", a.cost.SessionTotal()))

	providers := a.cost.ByProvider()
	if len(providers) == 0 {
		lines = append(lines, "No requests yet.")
		return strings.Join(lines, "\n")
	}

	order := []string{"claude", "codex", "gemini"}
	for _, provider := range order {
		cost, ok := providers[provider]
		if !ok {
			continue
		}
		requests := a.cost.RequestCount(provider)
		inTok, outTok := a.cost.TokensByProvider(provider)
		lines = append(lines, fmt.Sprintf("%s: $%.2f, %d requests, %d in / %d out tokens", provider, cost, requests, inTok, outTok))
	}

	return strings.Join(lines, "\n")
}

func classifyTask(input string) model.TaskType {
	return model.ClassifyTask(input)
}

func shouldUseUnaryChat(task model.TaskType) bool {
	switch task {
	case model.TaskExplain, model.TaskAnalyze:
		return true
	default:
		return false
	}
}

func chatTaskToBuildPhase(task model.TaskType) model.BuildPhase {
	switch task {
	case model.TaskCode:
		return model.PhaseCode
	case model.TaskReview:
		return model.PhaseReview
	case model.TaskFix:
		return model.PhaseFix
	default:
		return model.PhasePlan
	}
}
