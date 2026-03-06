package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

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
			a = a.applyBudgetRoutingPolicy()

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
		a = a.applyBudgetRoutingPolicy()
		m := i18n.Msg()

		cwd, err := os.Getwd()
		if err != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf(m.WizardErrWorkdir, err),
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
				Content: fmt.Sprintf(m.WizardErrProject, err),
			})
			return a, nil
		}
		a.project = proj

		if err := proj.GitInit(context.Background()); err != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf(m.WizardWarnGitInit, err),
			})
		}

		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: fmt.Sprintf(m.WizardProjectMade, proj.Name),
		})
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
				content, usage = retryWizardBuildForMissingFiles(ctx, router, prompt, content, usage)
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
			content, usage = retryWizardBuildForMissingFiles(ctx, router, prompt, content, usage)

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

// retryWizardBuildForMissingFiles performs one strict-format retry when the
// code-generation response contains no writable file blocks.
func retryWizardBuildForMissingFiles(
	ctx context.Context,
	router *model.Router,
	originalPrompt string,
	content string,
	usage model.Usage,
) (string, model.Usage) {
	if router == nil {
		return content, usage
	}
	if len(engine.ParseFilesBestEffort(content).Files) > 0 {
		return content, usage
	}

	retryPrompt := buildWizardCodeFormatRetryPrompt(originalPrompt, content)
	retryMessages := []model.Message{{Role: "user", Content: retryPrompt}}
	retrySystem := wizardBuildSystemPrompt + "\n\n" + wizardBuildRetryRules

	var (
		retryContent string
		retryUsage   model.Usage
		err          error
	)

	// Prefer retrying with the same provider that produced the non-file response.
	if router.ModeSet() {
		preferred := strings.TrimSpace(usage.Provider)
		if preferred != "" {
			retryContent, retryUsage, _, err = router.ChatWith(ctx, preferred, model.PhaseCode, retryMessages, retrySystem)
		}
		if preferred == "" || err != nil {
			retryContent, retryUsage, _, err = router.ChatBest(ctx, model.PhaseCode, retryMessages, retrySystem)
		}
	} else {
		retryContent, retryUsage, _, err = router.Chat(ctx, model.TaskCode, retryMessages, retrySystem)
	}
	if err != nil || len(engine.ParseFilesBestEffort(retryContent).Files) == 0 {
		return content, usage
	}

	total := usage
	total.InputTokens += retryUsage.InputTokens
	total.OutputTokens += retryUsage.OutputTokens
	total.Cost += retryUsage.Cost
	if retryUsage.Provider != "" {
		total.Provider = retryUsage.Provider
	}
	if retryUsage.Model != "" {
		total.Model = retryUsage.Model
	}

	return retryContent, total
}
