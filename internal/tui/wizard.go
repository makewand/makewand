package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/template"
)

// WizardPhase represents the current phase of the project wizard.
type WizardPhase int

const (
	WizardPhaseTemplate WizardPhase = iota
	WizardPhaseDescribe
	WizardPhasePlan
	WizardPhaseConfirm
	WizardPhaseBuild
	WizardPhaseDone
)

// WizardPanel guides non-programmers through project creation.
type WizardPanel struct {
	phase      WizardPhase
	templates  []template.Template
	selected   int
	customDesc string
	plan       string
	width      int
	height     int
}

// NewWizardPanel creates a new wizard panel.
func NewWizardPanel() WizardPanel {
	return WizardPanel{
		phase:     WizardPhaseTemplate,
		templates: template.All(),
	}
}

// Phase returns the current wizard phase.
func (w *WizardPanel) Phase() WizardPhase {
	return w.phase
}

// SetPhase sets the wizard phase.
func (w *WizardPanel) SetPhase(phase WizardPhase) {
	w.phase = phase
}

// SelectedTemplate returns the selected template, or nil if custom.
func (w *WizardPanel) SelectedTemplate() *template.Template {
	if w.selected >= 0 && w.selected < len(w.templates) {
		t := w.templates[w.selected]
		return &t
	}
	return nil
}

// CustomDescription returns the user's custom project description.
func (w *WizardPanel) CustomDescription() string {
	return w.customDesc
}

// SetCustomDescription sets the custom description.
func (w *WizardPanel) SetCustomDescription(desc string) {
	w.customDesc = desc
}

// SetPlan sets the AI-generated project plan.
func (w *WizardPanel) SetPlan(plan string) {
	w.plan = plan
}

// SetSize sets the panel dimensions.
func (w *WizardPanel) SetSize(width, height int) {
	w.width = width
	w.height = height
}

// Update handles key events for the wizard.
func (w WizardPanel) Update(msg tea.Msg) (WizardPanel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if w.phase == WizardPhaseTemplate {
			switch msg.String() {
			case "up", "k":
				if w.selected > 0 {
					w.selected--
				}
			case "down", "j":
				maxIdx := len(w.templates) // templates + "custom" option
				if w.selected < maxIdx {
					w.selected++
				}
			}
		}
	}
	return w, nil
}

// View renders the wizard panel.
func (w WizardPanel) View() string {
	msg := i18n.Msg()

	switch w.phase {
	case WizardPhaseTemplate:
		return w.viewTemplateSelect(msg)
	case WizardPhaseDescribe:
		return w.viewDescribe(msg)
	case WizardPhasePlan:
		return w.viewPlan(msg)
	case WizardPhaseConfirm:
		return w.viewConfirm(msg)
	case WizardPhaseBuild:
		return w.viewBuild(msg)
	case WizardPhaseDone:
		return w.viewDone(msg)
	default:
		return ""
	}
}

func (w WizardPanel) viewTemplateSelect(msg *i18n.Messages) string {
	var b strings.Builder

	b.WriteString(logoStyle.Render(logo) + "\n\n")
	b.WriteString(titleStyle.Render("🪄 "+msg.WizardWelcome) + "\n")
	b.WriteString(subtitleStyle.Render(msg.WizardTemplate) + "\n\n")

	for i, t := range w.templates {
		cursor := "  "
		name := t.Name
		if i18n.GetLanguage() == "zh" {
			name = t.NameZh
		}
		label := fmt.Sprintf("%s %s", t.Icon, name)

		if i == w.selected {
			cursor = "❯ "
			label = selectedStyle.Render(label)
		}

		b.WriteString(cursor + label + "\n")
	}

	// Custom option
	cursor := "  "
	label := fmt.Sprintf("💬 %s", msg.WizardCustom)
	if w.selected == len(w.templates) {
		cursor = "❯ "
		label = selectedStyle.Render(label)
	}
	b.WriteString(cursor + label + "\n")

	b.WriteString("\n" + helpStyle.Render(msg.WizardNavHint) + "\n")

	return b.String()
}

func (w WizardPanel) viewDescribe(msg *i18n.Messages) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("🪄 "+msg.WizardPrompt) + "\n\n")
	b.WriteString(helpStyle.Render(msg.WizardDescribeHint) + "\n")
	return b.String()
}

func (w WizardPanel) viewPlan(msg *i18n.Messages) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("📋 "+msg.WizardPlanning) + "\n\n")
	if w.plan != "" {
		b.WriteString(w.plan + "\n")
	} else {
		b.WriteString(spinnerStyle.Render("● ") + msg.WizardPlanning + "\n")
	}
	return b.String()
}

func (w WizardPanel) viewConfirm(msg *i18n.Messages) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("✅ "+msg.WizardConfirm) + "\n\n")
	if w.plan != "" {
		b.WriteString(w.plan + "\n\n")
	}
	b.WriteString(helpStyle.Render(msg.WizardConfirmHint) + "\n")
	return b.String()
}

func (w WizardPanel) viewBuild(msg *i18n.Messages) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("🔨 "+msg.WizardBuilding) + "\n\n")
	return b.String()
}

func (w WizardPanel) viewDone(msg *i18n.Messages) string {
	var b strings.Builder
	b.WriteString(successStyle.Render("🎉 "+msg.WizardDone) + "\n\n")
	return b.String()
}
