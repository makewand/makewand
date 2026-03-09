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
			a.quitting = true
			return a, tea.Quit
		}
	}

	a.chat.AddMessage(ChatMessage{Role: "user", Content: input})
	a.chat.SetStreaming(true)
	a = a.applyBudgetRoutingPolicy()

	messages := a.chat.ToModelMessages()
	task := classifyTask(input)
	systemPrompt := buildSystemPrompt(a.project, task)

	ctx, cancel := context.WithTimeout(context.Background(), chatStreamTimeout)
	a.cancelAI = cancel

	// In power mode, chat uses multi-model ensemble + judge (ChatBest).
	// Other modes keep low-latency stream-first behavior.
	if a.router.ModeSet() && a.router.Mode() == model.ModePower {
		phase := chatTaskToBuildPhase(task)
		cmd := func() tea.Msg {
			defer cancel()
			content, usage, result, err := a.router.ChatBest(ctx, phase, messages, systemPrompt)
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

	// Explain/analyze prompts are short and often hit local fallbacks such as Ollama.
	// Using unary chat here avoids getting stuck waiting for an unreliable stream.
	if shouldUseUnaryChat(task) {
		cmd := func() tea.Msg {
			defer cancel()
			content, usage, result, err := a.router.Chat(ctx, task, messages, systemPrompt)
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

	cmd := func() tea.Msg {
		defer cancel()
		ch, result, err := a.router.ChatStream(ctx, task, messages, systemPrompt)
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
	a.confirmingFiles = false
	a.confirmingDeps = false
	a.confirmingTests = false
	a.clearPendingApproval()
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
			Content: fmt.Sprintf("%s: %s\n/model [free|economy|balanced|power]", msg.ModeLabel, a.currentModeLabel()),
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
	case model.ModeFree:
		displayName = msg.ModeFree
	case model.ModeEconomy:
		displayName = msg.ModeEconomy
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
		"/approve - Approve the pending action",
		"/deny - Deny the pending action",
		"/resume - Restore the last saved session",
		"/model [free|economy|balanced|power] - Switch routing profile",
		"/exit - Quit makewand",
	}, "\n")
}

func (a App) statusSummary() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Model profile: %s", a.currentModeLabel()))

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
		lines = append(lines, "Pending approval:")
		lines = append(lines, summary)
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

	order := []string{"claude", "codex", "gemini", "openai", "ollama"}
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
