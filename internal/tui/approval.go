package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

type approvalKind string

const (
	approvalNone      approvalKind = ""
	approvalFileWrite approvalKind = "file_write"
	approvalDeps      approvalKind = "deps"
	approvalTests     approvalKind = "tests"
)

type approvalRequest struct {
	Kind    approvalKind
	Title   string
	Details string
}

func (a *App) setPendingApproval(kind approvalKind, title, details string) {
	a.pendingApproval = &approvalRequest{
		Kind:    kind,
		Title:   strings.TrimSpace(title),
		Details: strings.TrimSpace(details),
	}
}

func (a *App) clearPendingApproval() {
	a.pendingApproval = nil
}

func (a App) activeApprovalKind() approvalKind {
	if a.pendingApproval != nil && a.pendingApproval.Kind != approvalNone {
		return a.pendingApproval.Kind
	}
	switch a.state {
	case StateConfirmFiles:
		return approvalFileWrite
	case StateConfirmDeps:
		return approvalDeps
	case StateConfirmTests:
		return approvalTests
	default:
		return approvalNone
	}
}

func approvalActionHint() string {
	return i18n.Msg().ApprovalActionHint
}

func (a App) currentApprovalMode() string {
	if a.cfg == nil {
		return config.ApprovalModeManual
	}
	return config.NormalizeApprovalMode(a.cfg.ApprovalMode)
}

func (a *App) setApprovalMode(mode string) {
	if a.cfg == nil {
		return
	}
	a.cfg.ApprovalMode = config.NormalizeApprovalMode(mode)
}

func (a App) currentApprovalModeLabel() string {
	switch a.currentApprovalMode() {
	case config.ApprovalModeSafe:
		return i18n.Msg().ApprovalModeSafe
	case config.ApprovalModeAuto:
		return i18n.Msg().ApprovalModeAutopilot
	default:
		return i18n.Msg().ApprovalModeManual
	}
}

func (a App) safeApprovalEnabled() bool {
	mode := a.currentApprovalMode()
	return mode == config.ApprovalModeSafe || mode == config.ApprovalModeAuto
}

func (a App) shouldAutoApproveFileWrites(phase pendingPhaseType) bool {
	mode := a.currentApprovalMode()
	if !a.safeApprovalEnabled() || a.project == nil {
		return false
	}
	if mode == config.ApprovalModeAuto {
		switch phase {
		case pendingPhaseChat, pendingPhaseBuild, pendingPhaseFix:
			return a.pendingWriteVerified
		default:
			return false
		}
	}
	switch phase {
	case pendingPhaseChat, pendingPhaseReview, pendingPhaseFix:
		return true
	default:
		return false
	}
}

func (a App) shouldAutoApproveRestrictedPlan(plan *engine.ExecPlan) bool {
	return a.safeApprovalEnabled() && plan != nil
}

func (a *App) addAutoApprovalStatus(content string) {
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: content,
	})
}

func (a App) autoApprovedWriteStatus(count int) string {
	msg := i18n.Msg()
	if a.currentApprovalMode() == config.ApprovalModeAuto {
		return fmt.Sprintf(msg.ApprovalAutoWriteAutopilot, count)
	}
	return fmt.Sprintf(msg.ApprovalAutoWrite, count)
}

func (a App) pendingApprovalSummary() string {
	if a.pendingApproval == nil {
		return ""
	}

	lines := []string{a.pendingApproval.Title}
	if a.pendingApproval.Details != "" {
		lines = append(lines, a.pendingApproval.Details)
	}
	lines = append(lines, approvalActionHint())
	return strings.Join(lines, "\n")
}

func (a App) viewPendingApproval(width int) string {
	if a.pendingApproval == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString(i18n.Msg().ApprovalTitle + "\n")
	b.WriteString(wrapText(a.pendingApproval.Title, maxInt(width-6, 12)))
	if a.pendingApproval.Details != "" {
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(wrapText(a.pendingApproval.Details, maxInt(width-6, 12))))
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render(approvalActionHint()))

	return statusBorderStyle.
		Width(width - 2).
		Render(b.String())
}

func (a App) handleApproveCommand() (tea.Model, tea.Cmd) {
	switch a.activeApprovalKind() {
	case approvalFileWrite:
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmFileWriteMsg{confirmed: true}
		}
	case approvalDeps:
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmDepsInstallMsg{confirmed: true}
		}
	case approvalTests:
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmTestsRunMsg{confirmed: true}
		}
	default:
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().ApprovalNone,
		})
		return a, nil
	}
}

func (a App) handleDenyCommand() (tea.Model, tea.Cmd) {
	switch a.activeApprovalKind() {
	case approvalFileWrite:
		a.state = StateIdle
		a.clearPendingApproval()
		a.pendingFiles = nil
		a.pendingWriteVerified = false
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().FileCancelled,
		})
		return a, nil
	case approvalDeps:
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmDepsInstallMsg{confirmed: false}
		}
	case approvalTests:
		a.state = StateIdle
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmTestsRunMsg{confirmed: false}
		}
	default:
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().ApprovalNone,
		})
		return a, nil
	}
}

func approvalPrompt(title, details string) string {
	lines := []string{strings.TrimSpace(title)}
	if strings.TrimSpace(details) != "" {
		lines = append(lines, strings.TrimSpace(details))
	}
	lines = append(lines, approvalActionHint())
	return strings.Join(lines, "\n")
}

func pendingWriteDetails(count int) string {
	return fmt.Sprintf(i18n.Msg().ApprovalPendingWrite, count)
}

func plannedCommandDetails(command string) string {
	return fmt.Sprintf(i18n.Msg().ApprovalPlannedCommand, command)
}

func execCommandDetails(command string) string {
	return fmt.Sprintf(i18n.Msg().ExecCommand, command)
}

func execStartedMessage(label string, details string) string {
	msg := i18n.Msg()
	if strings.TrimSpace(details) == "" {
		return fmt.Sprintf(msg.ExecStarted, label)
	}
	return fmt.Sprintf(msg.ExecStarted, label) + "\n" + details
}

func execFinishedMessage(label string, details string, resultSummary string) string {
	lines := []string{fmt.Sprintf(i18n.Msg().ExecFinished, label)}
	if strings.TrimSpace(details) != "" {
		lines = append(lines, details)
	}
	if strings.TrimSpace(resultSummary) != "" {
		lines = append(lines, resultSummary)
	}
	return strings.Join(lines, "\n")
}

func execResultSummary(result *engine.ExecResult) string {
	if result == nil {
		return ""
	}

	msg := i18n.Msg()
	lines := []string{fmt.Sprintf(msg.ExecExitCode, result.ExitCode)}
	if result.Duration > 0 {
		lines = append(lines, fmt.Sprintf(msg.ExecDuration, result.Duration.Round(time.Millisecond)))
	}

	output := strings.TrimSpace(result.Stderr)
	if output == "" {
		output = strings.TrimSpace(result.Stdout)
	}
	if output != "" {
		runes := []rune(output)
		if len(runes) > 240 {
			output = string(runes[:240]) + "..."
		}
		lines = append(lines, fmt.Sprintf(msg.ExecOutput, output))
	}

	return strings.Join(lines, "\n")
}
