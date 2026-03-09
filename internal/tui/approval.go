package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	switch {
	case a.confirmingFiles:
		return approvalFileWrite
	case a.confirmingDeps:
		return approvalDeps
	case a.confirmingTests:
		return approvalTests
	default:
		return approvalNone
	}
}

func approvalActionHint() string {
	return "Use /approve or /deny (or Y/n)."
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
	b.WriteString("Pending Approval\n")
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
		a.confirmingFiles = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmFileWriteMsg{confirmed: true}
		}
	case approvalDeps:
		a.confirmingDeps = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmDepsInstallMsg{confirmed: true}
		}
	case approvalTests:
		a.confirmingTests = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmTestsRunMsg{confirmed: true}
		}
	default:
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: "No pending approval.",
		})
		return a, nil
	}
}

func (a App) handleDenyCommand() (tea.Model, tea.Cmd) {
	switch a.activeApprovalKind() {
	case approvalFileWrite:
		a.confirmingFiles = false
		a.clearPendingApproval()
		a.pendingFiles = nil
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().FileCancelled,
		})
		return a, nil
	case approvalDeps:
		a.confirmingDeps = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmDepsInstallMsg{confirmed: false}
		}
	case approvalTests:
		a.confirmingTests = false
		a.clearPendingApproval()
		return a, func() tea.Msg {
			return confirmTestsRunMsg{confirmed: false}
		}
	default:
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: "No pending approval.",
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

func execStartedMessage(label string, details string) string {
	if strings.TrimSpace(details) == "" {
		return fmt.Sprintf("Running %s", label)
	}
	return fmt.Sprintf("Running %s\n%s", label, details)
}

func execFinishedMessage(label string, details string, resultSummary string) string {
	lines := []string{fmt.Sprintf("%s finished", label)}
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

	lines := []string{fmt.Sprintf("Exit code: %d", result.ExitCode)}
	if result.Duration > 0 {
		lines = append(lines, fmt.Sprintf("Duration: %s", result.Duration.Round(time.Millisecond)))
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
		lines = append(lines, "Output: "+output)
	}

	return strings.Join(lines, "\n")
}
