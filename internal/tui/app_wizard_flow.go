package tui

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

func (a App) handleWizardEnter() (tea.Model, tea.Cmd) {
	phase := a.wizard.Phase()

	switch phase {
	case WizardPhaseTemplate:
		tpl := a.wizard.SelectedTemplate()
		if tpl != nil {
			a.wizard.SetPhase(WizardPhasePlan)

			ctx, cancel := context.WithCancel(context.Background())
			a.cancelAI = cancel
			router := a.router

			return a, func() tea.Msg {
				defer cancel()
				prompt := buildWizardPlanUserPrompt(tpl.Name, tpl.Prompt)
				messages := []model.Message{{Role: "user", Content: prompt}}

				// Use ChatBest: Power mode runs ensemble+judge; others use primary provider.
				if router.ModeSet() {
					content, usage, _, err := router.ChatBest(ctx, model.PhasePlan, messages, wizardPlanSystemPrompt)
					if err != nil {
						return aiResponseMsg{err: err}
					}
					return aiResponseMsg{
						content:      content,
						provider:     usage.Provider,
						cost:         usage.Cost,
						inputTokens:  usage.InputTokens,
						outputTokens: usage.OutputTokens,
					}
				}

				content, usage, _, err := router.Chat(ctx, model.TaskAnalyze, messages, wizardPlanSystemPrompt)
				if err != nil {
					return aiResponseMsg{err: err}
				}

				return aiResponseMsg{
					content:      content,
					provider:     usage.Provider,
					cost:         usage.Cost,
					inputTokens:  usage.InputTokens,
					outputTokens: usage.OutputTokens,
				}
			}
		}

		a.wizard.SetPhase(WizardPhaseDescribe)
		a.mode = ModeChat
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().WizardPrompt,
		})

	case WizardPhasePlan:
		a.wizard.SetPhase(WizardPhaseConfirm)

	case WizardPhaseConfirm:
		a.wizard.SetPhase(WizardPhaseBuild)
		a.mode = ModeChat

		cwd, err := os.Getwd()
		if err != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf("Error getting working directory: %s", err),
			})
			return a, nil
		}

		tpl := a.wizard.SelectedTemplate()
		projectName := "my-project"
		if tpl != nil {
			projectName = tpl.ID + "-project"
		}

		proj, err := engine.NewProject(projectName, cwd)
		if err != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf("Error creating project: %s", err),
			})
			return a, nil
		}
		a.project = proj

		if err := proj.GitInit(context.Background()); err != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf("Warning: git init failed: %s", err),
			})
		}

		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: fmt.Sprintf("Created project: %s", proj.Name),
		})

		m := i18n.Msg()
		a.progress.SetSteps([]ProgressStep{
			{Label: m.ProgressAnalyzing, Status: StepDone},
			{Label: m.ProgressCreating, Status: StepRunning},
			{Label: m.ProgressReviewing, Status: StepPending},
			{Label: m.ProgressInstallingDeps, Status: StepPending},
			{Label: m.ProgressTesting, Status: StepPending},
		})

		ctx, cancel := context.WithCancel(context.Background())
		a.cancelAI = cancel
		router := a.router

		return a, func() tea.Msg {
			defer cancel()
			var prompt string
			if tpl != nil {
				prompt = tpl.Prompt
			} else {
				prompt = a.wizard.CustomDescription()
			}

			messages := []model.Message{{Role: "user", Content: prompt}}

			// Use ChatBest: Power mode runs ensemble+judge; others use primary provider.
			if router.ModeSet() {
				content, usage, _, err := router.ChatBest(ctx, model.PhaseCode, messages, wizardBuildSystemPrompt)
				if err != nil {
					return aiResponseMsg{err: err}
				}
				return aiResponseMsg{
					content:      content,
					provider:     usage.Provider,
					cost:         usage.Cost,
					inputTokens:  usage.InputTokens,
					outputTokens: usage.OutputTokens,
				}
			}

			content, usage, _, err := router.Chat(ctx, model.TaskCode, messages, wizardBuildSystemPrompt)
			if err != nil {
				return aiResponseMsg{err: err}
			}

			return aiResponseMsg{
				content:      content,
				provider:     usage.Provider,
				cost:         usage.Cost,
				inputTokens:  usage.InputTokens,
				outputTokens: usage.OutputTokens,
			}
		}
	}

	return a, nil
}
