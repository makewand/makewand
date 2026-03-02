package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

// Mode represents the app's operating mode.
type Mode int

const (
	ModeChat   Mode = iota // makewand chat
	ModeNew                // makewand new (wizard)
)

// App is the main Bubble Tea model.
type App struct {
	mode     Mode
	cfg      *config.Config
	router   *model.Router
	project  *engine.Project

	// UI components
	chat     ChatPanel
	fileTree FileTreePanel
	progress ProgressPanel
	wizard   WizardPanel
	cost     *CostTracker
	spinner  spinner.Model

	// State
	width    int
	height   int
	quitting bool
	err      error
}

// AI response messages
type aiResponseMsg struct {
	content  string
	provider string
	cost     float64
	err      error
}

type aiStreamMsg struct {
	chunk model.StreamChunk
	provider string
}

type filesUpdatedMsg struct{}

// NewApp creates a new App.
func NewApp(mode Mode, cfg *config.Config, projectPath string) *App {
	router := model.NewRouter(cfg)
	i18n.SetLanguage(cfg.Language)

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = spinnerStyle

	app := &App{
		mode:     mode,
		cfg:      cfg,
		router:   router,
		chat:     NewChatPanel(),
		fileTree: NewFileTreePanel(),
		progress: NewProgressPanel(),
		wizard:   NewWizardPanel(),
		cost:     NewCostTracker(),
		spinner:  s,
	}

	// Open existing project if path provided
	if projectPath != "" {
		if proj, err := engine.OpenProject(projectPath); err == nil {
			app.project = proj
			app.fileTree.SetFiles(proj.Files)
		}
	}

	return app
}

// Init implements tea.Model.
func (a App) Init() tea.Cmd {
	return tea.Batch(
		a.spinner.Tick,
		tea.WindowSize(),
	)
}

// Update implements tea.Model.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height

		// Update component sizes
		chatWidth := a.width
		sideWidth := 0
		if a.width > 80 {
			sideWidth = 30
			chatWidth = a.width - sideWidth
		}
		sideHeight := a.height - 2

		a.fileTree.SetSize(sideWidth, sideHeight/2)
		a.progress.SetSize(sideWidth, sideHeight/2)
		a.wizard.SetSize(chatWidth, a.height)

		// Forward to chat panel
		chatMsg := tea.WindowSizeMsg{Width: chatWidth, Height: a.height - 2}
		var chatCmd tea.Cmd
		a.chat, chatCmd = a.chat.Update(chatMsg)
		cmds = append(cmds, chatCmd)

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			a.quitting = true
			return a, tea.Quit

		case "enter":
			if a.mode == ModeNew {
				return a.handleWizardEnter()
			}
			return a.handleChatEnter()

		case "q":
			if a.mode == ModeNew && a.wizard.Phase() == WizardPhaseTemplate {
				a.quitting = true
				return a, tea.Quit
			}

		case "esc":
			if a.mode == ModeNew && a.wizard.Phase() > WizardPhaseTemplate {
				a.wizard.SetPhase(a.wizard.Phase() - 1)
				return a, nil
			}
		}

		// Forward to wizard if in new mode
		if a.mode == ModeNew && a.wizard.Phase() == WizardPhaseTemplate {
			var wizCmd tea.Cmd
			a.wizard, wizCmd = a.wizard.Update(msg)
			cmds = append(cmds, wizCmd)
		}

	case aiResponseMsg:
		if msg.err != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf("Error: %s", msg.err),
			})
		} else {
			a.chat.AddMessage(ChatMessage{
				Role:     "assistant",
				Content:  msg.content,
				Provider: msg.provider,
				Cost:     msg.cost,
			})
			a.cost.Add(msg.provider, msg.cost)
		}
		a.chat.SetStreaming(false)

	case aiStreamMsg:
		if msg.chunk.Error != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf("Error: %s", msg.chunk.Error),
			})
			a.chat.SetStreaming(false)
		} else if msg.chunk.Done {
			a.chat.FinishStream(msg.provider, 0)
		} else {
			a.chat.AppendStream(msg.chunk.Content)
		}

	case filesUpdatedMsg:
		if a.project != nil {
			a.project.ScanFiles()
			a.fileTree.SetFiles(a.project.Files)
		}

	case spinner.TickMsg:
		var spinCmd tea.Cmd
		a.spinner, spinCmd = a.spinner.Update(msg)
		cmds = append(cmds, spinCmd)

		var progCmd tea.Cmd
		a.progress, progCmd = a.progress.Update(msg)
		cmds = append(cmds, progCmd)
	}

	// Update chat panel
	var chatCmd tea.Cmd
	a.chat, chatCmd = a.chat.Update(msg)
	cmds = append(cmds, chatCmd)

	return a, tea.Batch(cmds...)
}

// View implements tea.Model.
func (a App) View() string {
	if a.quitting {
		return mutedStyle.Render("Goodbye! 👋") + "\n"
	}

	if a.mode == ModeNew && a.wizard.Phase() <= WizardPhaseConfirm {
		return a.wizard.View()
	}

	return a.viewLayout()
}

func (a App) viewLayout() string {
	// Header
	header := a.viewHeader()

	// Main content
	if a.width <= 80 {
		// Narrow: stacked layout
		return header + "\n" + a.chat.View()
	}

	// Wide: side-by-side layout
	sideWidth := 30
	chatWidth := a.width - sideWidth

	// Chat panel (left)
	chatView := chatBorderStyle.
		Width(chatWidth - 2).
		Height(a.height - 4).
		Render(a.chat.View())

	// Side panel (right)
	sideView := a.viewSidePanel(sideWidth)

	main := lipgloss.JoinHorizontal(lipgloss.Top, chatView, sideView)

	return header + "\n" + main
}

func (a App) viewHeader() string {
	msg := i18n.Msg()

	left := logoStyle.Render("🪄 makewand")
	right := mutedStyle.Render(msg.Version)

	if len(a.router.Available()) > 0 {
		models := strings.Join(a.router.Available(), " | ")
		right = mutedStyle.Render(models) + "  " + right
	}

	gap := a.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}

	return left + strings.Repeat(" ", gap) + right
}

func (a App) viewSidePanel(width int) string {
	var sections []string

	// File tree
	sections = append(sections, a.fileTree.View())

	// Cost tracker
	sections = append(sections, a.cost.View(width))

	// Progress (if any steps)
	progView := a.progress.View()
	if progView != "" {
		sections = append(sections, progView)
	}

	return strings.Join(sections, "\n")
}

func (a App) handleChatEnter() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(a.chat.InputValue())
	if input == "" {
		return a, nil
	}

	// Add user message
	a.chat.AddMessage(ChatMessage{Role: "user", Content: input})
	a.chat.ClearInput()
	a.chat.SetStreaming(true)

	// Send to AI
	messages := a.chat.ToModelMessages()
	systemPrompt := buildSystemPrompt(a.project)

	cmd := func() tea.Msg {
		ctx := context.Background()
		provider, err := a.router.Route(model.TaskCode)
		if err != nil {
			return aiResponseMsg{err: err}
		}

		content, usage, err := provider.Chat(ctx, messages, systemPrompt)
		if err != nil {
			return aiResponseMsg{err: err}
		}

		return aiResponseMsg{
			content:  content,
			provider: usage.Provider,
			cost:     usage.Cost,
		}
	}

	return a, cmd
}

func (a App) handleWizardEnter() (tea.Model, tea.Cmd) {
	phase := a.wizard.Phase()

	switch phase {
	case WizardPhaseTemplate:
		tpl := a.wizard.SelectedTemplate()
		if tpl != nil {
			// Template selected, go to plan phase
			a.wizard.SetPhase(WizardPhasePlan)

			// Get AI to plan
			return a, func() tea.Msg {
				ctx := context.Background()
				provider, err := a.router.Route(model.TaskAnalyze)
				if err != nil {
					return aiResponseMsg{err: err}
				}

				prompt := fmt.Sprintf(
					"I want to create a project using this template: %s\n\nRequirements:\n%s\n\nPlease provide a brief project plan with:\n1. Tech stack choices\n2. Main features\n3. File structure\n4. Estimated cost\n\nKeep it concise and non-technical.",
					tpl.Name, tpl.Prompt,
				)

				messages := []model.Message{{Role: "user", Content: prompt}}
				content, usage, err := provider.Chat(ctx, messages, "You are a friendly project planner. Explain things simply for non-programmers.")
				if err != nil {
					return aiResponseMsg{err: err}
				}

				return aiResponseMsg{
					content:  content,
					provider: usage.Provider,
					cost:     usage.Cost,
				}
			}
		}

		// Custom description selected
		a.wizard.SetPhase(WizardPhaseDescribe)
		a.mode = ModeChat // Switch to chat mode for free-form input
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: i18n.Msg().WizardPrompt,
		})

	case WizardPhasePlan:
		// Plan shown, move to confirm
		a.wizard.SetPhase(WizardPhaseConfirm)

	case WizardPhaseConfirm:
		// Confirmed, start building
		a.wizard.SetPhase(WizardPhaseBuild)
		a.mode = ModeChat // Switch to chat for build progress

		// Create project directory
		cwd, _ := os.Getwd()
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

		// Initialize git
		proj.GitInit(context.Background())

		a.chat.AddMessage(ChatMessage{
			Role:    "status",
			Content: fmt.Sprintf("Created project at %s", proj.Path),
		})

		// Set up progress steps
		a.progress.SetSteps([]ProgressStep{
			{Label: "Plan project", Status: StepDone},
			{Label: "Generate code", Status: StepRunning},
			{Label: "Install dependencies", Status: StepPending},
			{Label: "Run tests", Status: StepPending},
		})

		// Send build request to AI
		return a, func() tea.Msg {
			ctx := context.Background()
			provider, err := a.router.Route(model.TaskCode)
			if err != nil {
				return aiResponseMsg{err: err}
			}

			prompt := ""
			if tpl != nil {
				prompt = tpl.Prompt
			} else {
				prompt = a.wizard.CustomDescription()
			}

			messages := []model.Message{{Role: "user", Content: prompt}}
			content, usage, err := provider.Chat(ctx, messages,
				"You are an expert programmer. Generate a complete, working project. Output each file with its path and content in this format:\n\n--- FILE: path/to/file ---\n```\nfile content here\n```\n\nGenerate ALL files needed for a working project.")
			if err != nil {
				return aiResponseMsg{err: err}
			}

			return aiResponseMsg{
				content:  content,
				provider: usage.Provider,
				cost:     usage.Cost,
			}
		}
	}

	return a, nil
}

func buildSystemPrompt(project *engine.Project) string {
	prompt := `You are makewand, a friendly AI coding assistant for non-programmers.

Guidelines:
- Explain everything in simple, non-technical language
- When creating or modifying files, show the file path and content clearly
- When something goes wrong, explain what happened and fix it
- Always confirm before making major changes
- Be encouraging and supportive`

	if project != nil {
		prompt += fmt.Sprintf("\n\nCurrent project: %s\nProject path: %s\n", project.Name, project.Path)

		if tree := project.FileTree(); tree != "" {
			prompt += "\nProject files:\n" + tree
		}
	}

	return prompt
}

// Run starts the Bubble Tea program.
func Run(mode Mode, cfg *config.Config, projectPath string) error {
	app := NewApp(mode, cfg, projectPath)
	p := tea.NewProgram(app, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
