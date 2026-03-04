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

	// Handle /mode command locally (don't send to AI).
	if strings.HasPrefix(strings.ToLower(input), "/mode") {
		return a.handleModeCommand(input)
	}

	a.chat.AddMessage(ChatMessage{Role: "user", Content: input})
	a.chat.SetStreaming(true)
	a = a.applyBudgetRoutingPolicy()

	messages := a.chat.ToModelMessages()
	systemPrompt := buildSystemPrompt(a.project)
	task := classifyTask(input)

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

func (a App) handleModeCommand(input string) (tea.Model, tea.Cmd) {
	msg := i18n.Msg()
	parts := strings.Fields(input)

	if len(parts) == 1 {
		// /mode with no argument: show current mode and help
		var current string
		if a.router.ModeSet() {
			current = a.router.Mode().String()
		} else {
			current = "not set (legacy routing)"
		}
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("%s: %s\n%s", msg.ModeLabel, current, msg.ModeHelp),
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
		Content: fmt.Sprintf(msg.ModeChanged, displayName),
	})
	return a, nil
}

// classifyTask determines the TaskType from user input using prefix commands and keyword heuristics.
func classifyTask(input string) model.TaskType {
	lower := strings.ToLower(input)

	// Prefix commands
	if strings.HasPrefix(lower, "/review") {
		return model.TaskReview
	}
	if strings.HasPrefix(lower, "/fix") {
		return model.TaskFix
	}
	if strings.HasPrefix(lower, "/ask") || strings.HasPrefix(lower, "/explain") {
		return model.TaskExplain
	}
	if strings.HasPrefix(lower, "/plan") {
		return model.TaskAnalyze
	}

	// Keyword heuristics
	reviewKeywords := []string{"review", "check", "audit"}
	for _, kw := range reviewKeywords {
		if strings.Contains(lower, kw) {
			return model.TaskReview
		}
	}

	fixKeywords := []string{"fix", "bug", "error"}
	for _, kw := range fixKeywords {
		if strings.Contains(lower, kw) {
			return model.TaskFix
		}
	}

	explainKeywords := []string{"explain", "why", "how does"}
	for _, kw := range explainKeywords {
		if strings.Contains(lower, kw) {
			return model.TaskExplain
		}
	}

	analyzeKeywords := []string{"plan", "analyze", "design"}
	for _, kw := range analyzeKeywords {
		if strings.Contains(lower, kw) {
			return model.TaskAnalyze
		}
	}

	return model.TaskCode
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
