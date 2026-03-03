package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/i18n"
)

// ProgressStep represents a step in the build process.
type ProgressStep struct {
	Label  string
	Detail string
	Status StepStatus
}

// StepStatus represents the state of a progress step.
type StepStatus int

const (
	StepPending StepStatus = iota
	StepRunning
	StepDone
	StepFailed
)

// ProgressPanel shows the current build progress.
type ProgressPanel struct {
	steps   []ProgressStep
	spinner spinner.Model
	width   int
	height  int
}

// NewProgressPanel creates a new progress panel.
func NewProgressPanel() ProgressPanel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = spinnerStyle

	return ProgressPanel{
		spinner: s,
	}
}

// SetSteps sets the progress steps.
func (p *ProgressPanel) SetSteps(steps []ProgressStep) {
	p.steps = steps
}

// SetStepStatus updates a step's status.
func (p *ProgressPanel) SetStepStatus(index int, status StepStatus) {
	if index >= 0 && index < len(p.steps) {
		p.steps[index].Status = status
	}
}

// SetStepDetail updates a step's detail text.
func (p *ProgressPanel) SetStepDetail(index int, detail string) {
	if index >= 0 && index < len(p.steps) {
		p.steps[index].Detail = detail
	}
}

// AddStep adds a new step.
func (p *ProgressPanel) AddStep(label, detail string) {
	p.steps = append(p.steps, ProgressStep{
		Label:  label,
		Detail: detail,
		Status: StepPending,
	})
}

// SetSize sets the panel dimensions.
func (p *ProgressPanel) SetSize(width, height int) {
	p.width = width
	p.height = height
}

// Init returns the spinner tick command.
func (p ProgressPanel) Init() tea.Cmd {
	return p.spinner.Tick
}

// Update handles spinner updates.
func (p ProgressPanel) Update(msg tea.Msg) (ProgressPanel, tea.Cmd) {
	var cmd tea.Cmd
	p.spinner, cmd = p.spinner.Update(msg)
	return p, cmd
}

// View renders the progress panel.
func (p ProgressPanel) View() string {
	if len(p.steps) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("📋 "+i18n.Msg().ProgressTitle) + "\n")

	for _, step := range p.steps {
		var icon string
		switch step.Status {
		case StepPending:
			icon = mutedStyle.Render("○")
		case StepRunning:
			icon = p.spinner.View()
		case StepDone:
			icon = successStyle.Render("✓")
		case StepFailed:
			icon = errorStyle.Render("✗")
		}

		label := step.Label
		if step.Status == StepRunning {
			label = selectedStyle.Render(label)
		} else if step.Status == StepDone {
			label = successStyle.Render(label)
		} else if step.Status == StepFailed {
			label = errorStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}

		b.WriteString(fmt.Sprintf(" %s %s\n", icon, label))

		if step.Detail != "" && step.Status == StepRunning {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("   %s", step.Detail)) + "\n")
		}
	}

	return statusBorderStyle.Width(p.width - 2).Render(b.String())
}

// CompletedCount returns how many steps are done.
func (p *ProgressPanel) CompletedCount() int {
	count := 0
	for _, s := range p.steps {
		if s.Status == StepDone {
			count++
		}
	}
	return count
}

// TotalCount returns the total number of steps.
func (p *ProgressPanel) TotalCount() int {
	return len(p.steps)
}
