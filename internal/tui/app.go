package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/diag"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

// maxAutoFixRetries is a local alias for the pipeline constant, used in i18n
// messages and progress labels that reference the retry limit.
var maxAutoFixRetries = engine.MaxAutoFixRetries

// Build pipeline step indices (5-step progress).
const (
	stepPlan   = 0
	stepCode   = 1
	stepReview = 2
	stepDeps   = 3
	stepTests  = 4
)

// Mode represents the app's operating mode.
type Mode int

const (
	ModeChat Mode = iota // makewand chat
	ModeNew              // makewand new (wizard)
)

// pendingPhaseType distinguishes which pipeline phase is waiting for file confirmation.
type pendingPhaseType int

const (
	pendingPhaseNone   pendingPhaseType = iota
	pendingPhaseBuild                   // build wizard pipeline
	pendingPhaseChat                    // interactive chat
	pendingPhaseReview                  // code review fix files
	pendingPhaseFix                     // auto-fix files
)

// App is the main Bubble Tea model.
type App struct {
	mode    Mode
	cfg     *config.Config
	router  *model.Router
	project *engine.Project
	// initialPrompt is optionally auto-submitted when chat mode starts.
	initialPrompt string

	// UI components
	chat     ChatPanel
	fileTree FileTreePanel
	progress ProgressPanel
	wizard   WizardPanel
	cost     *CostTracker
	spinner  spinner.Model

	// Streaming state
	streamCh   <-chan model.StreamChunk
	streamProv string

	// Cancellable AI context
	cancelAI context.CancelFunc

	// Build pipeline domain logic (phase transitions, retry counting, provider tracking).
	pipeline *engine.BuildPipeline

	// Build pipeline TUI state (files, plans, approvals — owned by TUI layer).
	pendingFiles     []engine.ExtractedFile // files waiting to be written
	pendingPhase     pendingPhaseType       // which phase triggered the pending files
	state            AppState               // current interaction state
	pendingDepsPlan  *engine.ExecPlan       // detected dependency install command
	pendingTestsPlan *engine.ExecPlan       // detected test execution command
	pendingApproval  *approvalRequest       // current approval request, if any

	// State
	width  int
	height int
	err    error

	// Last budget warning level that has been surfaced to the user.
	lastBudgetNoticeLevel BudgetLevel

	// Debug route diagnostics (enabled by --debug).
	debugRoute *routeDebugState

	// Chat session metadata.
	sessionFile          string
	lastSessionSavedAt   string
	restoredSession      bool
	restoredMessageCount int
}

// --- Bubble Tea message types ---

type aiResponseMsg struct {
	content      string
	provider     string
	cost         float64
	inputTokens  int
	outputTokens int
	err          error
}

type aiStreamStartMsg struct {
	ch         <-chan model.StreamChunk
	provider   string
	isFallback bool
	requested  string
}

type aiStreamMsg struct {
	chunk    model.StreamChunk
	provider string
}

type filesUpdatedMsg struct{}

// filesExtractedMsg is sent after AI response text is parsed for file blocks.
type filesExtractedMsg struct {
	files []engine.ExtractedFile
	phase pendingPhaseType
}

// confirmFileWriteMsg is sent when user confirms/declines file writing.
type confirmFileWriteMsg struct {
	confirmed bool
}

// fileWriteCompleteMsg is sent after files are written to disk.
type fileWriteCompleteMsg struct {
	written int
	failed  int
	errors  []string
}

// depsInstallMsg is sent after dependency installation completes.
type depsInstallMsg struct {
	result *engine.ExecResult
	err    error
}

// confirmDepsInstallMsg is sent when user confirms/declines dependency installation.
type confirmDepsInstallMsg struct {
	confirmed bool
}

// confirmTestsRunMsg is sent when user confirms/declines test execution.
type confirmTestsRunMsg struct {
	confirmed bool
}

// testRunMsg is sent after tests complete.
type testRunMsg struct {
	result *engine.ExecResult
	err    error
	noTest bool // true when no test framework detected
}

// autoFixMsg triggers an auto-fix attempt.
type autoFixMsg struct {
	errOutput string
	attempt   int
}

// autoFixResponseMsg is sent after the AI responds with a fix.
type autoFixResponseMsg struct {
	content      string
	provider     string
	cost         float64
	inputTokens  int
	outputTokens int
	attempt      int
	err          error
}

// codeReviewMsg is sent after the review AI finishes analyzing code.
type codeReviewMsg struct {
	content      string
	provider     string
	cost         float64
	inputTokens  int
	outputTokens int
	hasIssues    bool // true if review found fixable issues
	err          error
}

// progressUpdateMsg updates a progress step.
type progressUpdateMsg struct {
	index  int
	status StepStatus
	detail string
}

// resumeTestsPhaseMsg asks the app to enter test phase after auto-fix/deps stages.
type resumeTestsPhaseMsg struct{}

// startPromptMsg submits an initial chat input at startup.
type startPromptMsg struct {
	input string
}

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
		pipeline: engine.NewBuildPipeline(),
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

	if mode == ModeChat {
		app.chat.ResetMessages([]ChatMessage{app.chatWelcomeMessage()})
		if app.project != nil {
			if sessionFile, err := chatSessionFilePath(app.project.Path); err == nil {
				app.sessionFile = sessionFile
			}
		}
	}

	return app
}

func (a App) chatWelcomeMessage() ChatMessage {
	msg := i18n.Msg()
	return ChatMessage{
		Role: "system",
		Content: fmt.Sprintf("%s\n%s\n%s",
			msg.ChatWelcome,
			msg.ChatPrompt,
			msg.ChatCommandHint,
		),
	}
}

// Init implements tea.Model.
func (a App) Init() tea.Cmd {
	cmds := []tea.Cmd{
		a.spinner.Tick,
		tea.WindowSize(),
	}
	if a.mode == ModeChat {
		if prompt := strings.TrimSpace(a.initialPrompt); prompt != "" {
			cmds = append(cmds, func() tea.Msg {
				return startPromptMsg{input: prompt}
			})
		}
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height

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

		chatMsg := tea.WindowSizeMsg{Width: chatWidth, Height: a.height - 2}
		var chatCmd tea.Cmd
		a.chat, chatCmd = a.chat.Update(chatMsg)
		cmds = append(cmds, chatCmd)

	case tea.KeyMsg:
		// Handle file write confirmation
		if a.state == StateConfirmFiles {
			return a.handleFileConfirmKey(msg)
		}
		// Handle dependency install confirmation
		if a.state == StateConfirmDeps {
			return a.handleDepsConfirmKey(msg)
		}
		// Handle test execution confirmation
		if a.state == StateConfirmTests {
			return a.handleTestsConfirmKey(msg)
		}

		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			if a.cancelAI != nil {
				a.cancelAI()
				a.cancelAI = nil
			}
			a.state = StateQuitting
			return a, tea.Quit

		case "ctrl+l":
			return a, tea.ClearScreen

		case "enter":
			if a.mode == ModeNew {
				return a.handleWizardEnter()
			}
			return a.handleChatEnter()

		case "q":
			if a.mode == ModeNew && a.wizard.Phase() == WizardPhaseTemplate {
				a.state = StateQuitting
				return a, tea.Quit
			}

		case "esc":
			if a.mode == ModeNew && a.wizard.Phase() > WizardPhaseTemplate {
				a.wizard.SetPhase(a.wizard.Phase() - 1)
				return a, nil
			}
		}

		if a.mode == ModeNew && a.wizard.Phase() == WizardPhaseTemplate {
			var wizCmd tea.Cmd
			a.wizard, wizCmd = a.wizard.Update(msg)
			cmds = append(cmds, wizCmd)
		}

	case aiResponseMsg:
		return a.handleAIResponse(msg)

	case startPromptMsg:
		return a.submitChatInput(msg.input)

	case aiStreamStartMsg:
		a.streamCh = msg.ch
		a.streamProv = msg.provider

		if msg.isFallback {
			m := i18n.Msg()
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf(m.FallbackNotice, msg.requested, msg.provider),
			})
		}

		cmds = append(cmds, a.waitForStreamChunk())

	case aiStreamMsg:
		if msg.chunk.Error != nil {
			a.chat.AddMessage(ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf("Error: %s", msg.chunk.Error),
			})
			a.chat.SetStreaming(false)
			a.streamCh = nil
		} else if msg.chunk.Done {
			a.chat.FinishStream(msg.provider, 0)
			a.streamCh = nil
			// After stream finishes, check for files in the response
			content := a.chat.LastAssistantContent()
			if content != "" && a.project != nil {
				phase := pendingPhaseChat
				if a.wizard.Phase() == WizardPhaseBuild {
					phase = pendingPhaseBuild
				}
				if engine.ContainsFiles(content) {
					cmds = append(cmds, func() tea.Msg {
						result := engine.ParseFiles(content)
						if len(result.Files) > 0 {
							return filesExtractedMsg{files: result.Files, phase: phase}
						}
						return nil
					})
				}
			}
		} else {
			a.chat.AppendStream(msg.chunk.Content)
			cmds = append(cmds, a.waitForStreamChunk())
		}

	case filesExtractedMsg:
		return a.handleFilesExtracted(msg)

	case confirmFileWriteMsg:
		return a.handleFileWriteConfirm(msg)

	case fileWriteCompleteMsg:
		return a.handleFileWriteComplete(msg)

	case depsInstallMsg:
		return a.handleDepsInstall(msg)

	case confirmDepsInstallMsg:
		return a.handleDepsInstallConfirm(msg)

	case confirmTestsRunMsg:
		return a.handleTestsRunConfirm(msg)

	case testRunMsg:
		return a.handleTestRun(msg)

	case autoFixMsg:
		return a.handleAutoFix(msg)

	case autoFixResponseMsg:
		return a.handleAutoFixResponse(msg)

	case codeReviewMsg:
		return a.handleCodeReview(msg)

	case progressUpdateMsg:
		a.progress.SetStepStatus(msg.index, msg.status)
		if msg.detail != "" {
			a.progress.SetStepDetail(msg.index, msg.detail)
		}

	case resumeTestsPhaseMsg:
		return a.startTestsPhase()

	case filesUpdatedMsg:
		a.refreshProjectFiles()

	case spinner.TickMsg:
		var spinCmd tea.Cmd
		a.spinner, spinCmd = a.spinner.Update(msg)
		cmds = append(cmds, spinCmd)

		var progCmd tea.Cmd
		a.progress, progCmd = a.progress.Update(msg)
		cmds = append(cmds, progCmd)
	}

	var chatCmd tea.Cmd
	a.chat, chatCmd = a.chat.Update(msg)
	cmds = append(cmds, chatCmd)

	return a, tea.Batch(cmds...)
}

// waitForStreamChunk reads the next chunk from the stream channel.
func (a App) waitForStreamChunk() tea.Cmd {
	ch := a.streamCh
	prov := a.streamProv
	return func() tea.Msg {
		chunk, ok := <-ch
		if !ok {
			return aiStreamMsg{chunk: model.StreamChunk{Done: true}, provider: prov}
		}
		return aiStreamMsg{chunk: chunk, provider: prov}
	}
}

// buildComplete marks the build as done.
func (a App) buildComplete() (tea.Model, tea.Cmd) {
	m := i18n.Msg()
	a.wizard.SetPhase(WizardPhaseDone)
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: m.ProgressBuildComplete,
	})
	return a, func() tea.Msg {
		return filesUpdatedMsg{}
	}
}

func (a *App) refreshProjectFiles() {
	if a.project == nil {
		return
	}
	if err := a.project.ScanFiles(); err != nil {
		a.chat.AddMessage(ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf(i18n.Msg().ErrFileRefresh, err),
		})
		return
	}
	a.fileTree.SetFiles(a.project.Files)
}

// View implements tea.Model.
func (a App) View() string {
	if a.state == StateQuitting {
		return mutedStyle.Render("Goodbye!") + "\n"
	}

	if a.mode == ModeNew && a.wizard.Phase() <= WizardPhaseConfirm {
		return a.wizard.View()
	}

	return a.viewLayout()
}

func (a App) viewLayout() string {
	header := a.viewHeader()

	if a.width <= 80 {
		return header + "\n" + a.chat.View()
	}

	sideWidth := 30
	chatWidth := a.width - sideWidth

	chatView := chatBorderStyle.
		Width(chatWidth - 2).
		Height(a.height - 4).
		Render(a.chat.View())

	sideView := a.viewSidePanel(sideWidth)

	main := lipgloss.JoinHorizontal(lipgloss.Top, chatView, sideView)

	return header + "\n" + main
}

func (a App) viewHeader() string {
	msg := i18n.Msg()

	left := logoStyle.Render("makewand")

	// Show mode badge if set
	if a.router.ModeSet() {
		badge := a.renderModeBadge(msg)
		left = left + " " + badge
	}

	right := mutedStyle.Render(msg.Version)

	if avail := a.router.Available(); len(avail) > 0 {
		models := strings.Join(avail, " | ")
		right = mutedStyle.Render(models) + "  " + right
	}

	gap := a.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}

	return left + strings.Repeat(" ", gap) + right
}

func (a App) renderModeBadge(msg *i18n.Messages) string {
	mode := a.router.Mode()
	var label string
	var style lipgloss.Style

	switch mode {
	case model.ModeFast:
		label = msg.ModeFast
		style = modeBadgeFastStyle
	case model.ModeBalanced:
		label = msg.ModeBalanced
		style = modeBadgeBalancedStyle
	case model.ModePower:
		label = msg.ModePower
		style = modeBadgePowerStyle
	default:
		return ""
	}

	return style.Render(label)
}

func (a App) viewSidePanel(width int) string {
	var sections []string

	sections = append(sections, a.fileTree.View())
	sections = append(sections, a.cost.View(width))
	if approvalView := a.viewPendingApproval(width); approvalView != "" {
		sections = append(sections, approvalView)
	}

	progView := a.progress.View()
	if progView != "" {
		sections = append(sections, progView)
	}
	if a.debugRoute != nil {
		if routeSummary := strings.TrimSpace(a.debugRoute.Summary()); routeSummary != "" {
			debugView := statusBorderStyle.
				Width(width - 2).
				Render("Debug Route\n" + wrapText(routeSummary, maxInt(width-6, 12)))
			sections = append(sections, debugView)
		}
	}

	return strings.Join(sections, "\n")
}

func isLGTMResponse(content string) bool {
	return strings.EqualFold(strings.TrimSpace(content), "LGTM")
}

// Run starts the Bubble Tea program.
func Run(mode Mode, cfg *config.Config, projectPath string, debug bool) error {
	return RunWithPrompt(mode, cfg, projectPath, "", debug)
}

// RunWithPrompt starts the Bubble Tea program with an optional initial chat prompt.
func RunWithPrompt(mode Mode, cfg *config.Config, projectPath, initialPrompt string, debug bool) error {
	app := NewApp(mode, cfg, projectPath)
	app.initialPrompt = strings.TrimSpace(initialPrompt)

	if debug {
		routeState := newRouteDebugState()
		app.debugRoute = routeState

		var candidates []string
		if dir, err := config.ConfigDir(); err == nil {
			candidates = append(candidates, filepath.Join(dir, "trace.jsonl"))
		}
		candidates = append(candidates, filepath.Join(os.TempDir(), "makewand-trace.jsonl"))

		fileSink, tracePath, sinkErr := diag.OpenFirstJSONLTraceSink(candidates)
		traceSink := &debugTraceSink{
			file:  fileSink,
			route: routeState,
		}
		app.router.SetTraceSink(traceSink)
		defer traceSink.Close()

		if fileSink == nil {
			diag.Stderr().WarnErr("debug trace disabled", sinkErr)
		} else {
			diag.Stderr().InfoPath("Debug trace enabled", tracePath)
		}
	}

	// Load cross-session routing quality statistics.
	if dir, err := config.ConfigDir(); err == nil {
		if err := app.router.LoadStats(dir); err != nil {
			diag.Stderr().WarnErr("could not load routing stats", err)
		}
	}
	if mode == ModeChat {
		if _, err := app.restoreChatSession(); err != nil {
			diag.Stderr().WarnErr("could not restore chat session", err)
		}
	}

	p := tea.NewProgram(app, tea.WithAltScreen())
	finalModel, err := p.Run()

	// Save routing quality statistics so the next session inherits learned preferences.
	if dir, dirErr := config.ConfigDir(); dirErr == nil {
		if finalApp, ok := finalModel.(App); ok {
			if err := finalApp.router.SaveStats(dir); err != nil {
				diag.Stderr().WarnErr("could not save routing stats", err)
			}
			if err := finalApp.saveChatSession(); err != nil {
				diag.Stderr().WarnErr("could not save chat session", err)
			}
		}
	}

	return err
}
