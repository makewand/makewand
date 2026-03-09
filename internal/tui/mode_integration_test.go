package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newBuildAppWithMode creates an App set up for the build pipeline with the
// given usage mode. It reuses the pattern from newBuildAppForDepsTest but also
// injects a Router wired to stub providers for the specified mode.
func newBuildAppWithMode(t *testing.T, mode model.UsageMode) App {
	t.Helper()

	cfg := config.DefaultConfig()
	appPtr := NewApp(ModeNew, cfg, "")
	app := *appPtr

	project, err := engine.NewProject("mode-integ-test", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := project.WriteFile("package.json", `{"name":"demo","scripts":{"test":"echo ok"}}`); err != nil {
		t.Fatalf("WriteFile(package.json): %v", err)
	}
	app.project = project
	app.mode = ModeChat // switch to chat-visible mode for pipeline

	// Inject a minimal router with stub providers.
	app.router = makeTestRouter(t, mode)

	// Set wizard to build phase with standard progress steps.
	app.wizard.SetPhase(WizardPhaseBuild)
	msg := i18n.Msg()
	app.progress.SetSteps([]ProgressStep{
		{Label: msg.ProgressAnalyzing, Status: StepDone},
		{Label: msg.ProgressCreating, Status: StepPending},
		{Label: msg.ProgressReviewing, Status: StepPending},
		{Label: msg.ProgressInstallingDeps, Status: StepPending},
		{Label: msg.ProgressTesting, Status: StepPending},
	})

	return app
}

// makeTestRouter builds a *model.Router with stub providers for all well-known
// providers, wired for the given mode. Since model.Router fields are internal,
// we use NewRouter with an empty config and then call SetMode + RegisterProvider.
func makeTestRouter(t *testing.T, mode model.UsageMode) *model.Router {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.UsageMode = mode.String()
	r := model.NewRouter(cfg)
	r.SetMode(mode)
	return r
}

// buildAIResponseWithFiles constructs an aiResponseMsg whose content contains
// "--- FILE: path ---" blocks so engine.ContainsFiles/ParseFiles picks them up.
func buildAIResponseWithFiles(provider string, paths ...string) aiResponseMsg {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "--- FILE: %s ---\n```\nconsole.log('hello');\n```\n\n", p)
	}
	return aiResponseMsg{
		content:  b.String(),
		provider: provider,
		cost:     0.01,
	}
}

// driveMessages applies a sequence of tea.Msg values through app.Update,
// returning the final App value.
func driveMessages(t *testing.T, app App, msgs ...tea.Msg) App {
	t.Helper()
	for i, msg := range msgs {
		m, _ := app.Update(msg)
		var ok bool
		app, ok = m.(App)
		if !ok {
			t.Fatalf("driveMessages: Update #%d returned %T, want App", i, m)
		}
	}
	return app
}

// ---------------------------------------------------------------------------
// Build Pipeline Tests
// ---------------------------------------------------------------------------

func TestMode_FreeBuild_UsesGemini(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeFree)

	// Simulate AI code generation response from gemini.
	resp := buildAIResponseWithFiles("gemini", "index.js")
	app = driveMessages(t, app, resp)

	if app.buildCodeProvider != "gemini" {
		t.Errorf("buildCodeProvider = %q, want %q", app.buildCodeProvider, "gemini")
	}
}

func TestMode_FreeBuild_NoAPIFallbackOnError(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeFree)

	// Simulate AI error.
	errResp := aiResponseMsg{err: fmt.Errorf("gemini rate limited")}
	app = driveMessages(t, app, errResp)

	// Should have an error message in chat, pipeline halted.
	lastMsg := app.chat.messages[len(app.chat.messages)-1]
	if !strings.Contains(lastMsg.Content, "gemini rate limited") {
		t.Errorf("expected error message, got %q", lastMsg.Content)
	}
}

func TestMode_EconomyBuild_ReviewLGTM(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeEconomy)
	app.buildCodeProvider = "gemini"

	// Code step already done; simulate review returning LGTM.
	review := codeReviewMsg{
		content:  "LGTM",
		provider: "claude",
		cost:     0.01,
	}
	app = driveMessages(t, app, review)

	if app.progress.steps[stepReview].Status != StepDone {
		t.Errorf("review step status = %v, want StepDone", app.progress.steps[stepReview].Status)
	}
}

func TestMode_BalancedBuild_ReviewFindsIssues(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)
	app.buildCodeProvider = "claude"

	// Review returns fix files.
	fixContent := "Found issues:\n--- FILE: index.js ---\n```\nconsole.log('fixed');\n```\n"
	review := codeReviewMsg{
		content:   fixContent,
		provider:  "gemini",
		cost:      0.02,
		hasIssues: true,
	}

	m, cmd := app.Update(review)
	app = m.(App)

	// Should produce a filesExtractedMsg command for review fixes.
	if cmd == nil {
		t.Fatal("expected a command after review with issues")
	}
	msg := cmd()
	extracted, ok := msg.(filesExtractedMsg)
	if !ok {
		t.Fatalf("command produced %T, want filesExtractedMsg", msg)
	}
	if extracted.phase != pendingPhaseReview {
		t.Errorf("extracted.phase = %v, want pendingPhaseReview", extracted.phase)
	}
}

func TestMode_BalancedBuild_ReviewFixDeclined(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)
	app.buildCodeProvider = "claude"

	// Simulate review fix files arriving.
	app = driveMessages(t, app, filesExtractedMsg{
		files: []engine.ExtractedFile{{Path: "fix.js", Content: "fixed"}},
		phase: pendingPhaseReview,
	})

	if app.state != StateConfirmFiles {
		t.Fatal("expected state=StateConfirmFiles for review fix")
	}

	// User declines.
	m, _ := app.handleFileConfirmKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	app = m.(App)

	if app.state == StateConfirmFiles {
		t.Error("state should not be StateConfirmFiles after decline")
	}
}

func TestMode_PowerBuild_SingleProviderSkipsReview(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModePower)
	// Simulate only 1 available provider by not registering any CLI/API providers.
	// NewRouter with default config has no providers → Available() returns empty.
	// We keep the default router which has no providers.
	app.router = model.NewRouter(config.DefaultConfig())
	app.buildCodeProvider = "claude"

	// Simulate file write complete in build phase.
	app.pendingPhase = pendingPhaseBuild
	m, _ := app.handleFileWriteComplete(fileWriteCompleteMsg{written: 3})
	app = m.(App)

	// With <= 1 available provider, review should be skipped.
	if app.progress.steps[stepReview].Status != StepDone {
		t.Errorf("review step status = %v, want StepDone (skipped)", app.progress.steps[stepReview].Status)
	}
	if !strings.Contains(app.progress.steps[stepReview].Detail, "single provider") {
		t.Errorf("review detail = %q, want 'single provider' note", app.progress.steps[stepReview].Detail)
	}
}

// ---------------------------------------------------------------------------
// Auto-Fix Cycle Tests
// ---------------------------------------------------------------------------

func TestMode_AutoFixCycle_SuccessAfterOneRetry(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)
	app.buildCodeProvider = "claude"

	// Simulate auto-fix attempt 1 triggering.
	app.autoFixAttempt = 1

	// Auto-fix response with fix files.
	fixContent := "--- FILE: index.js ---\n```\nconsole.log('auto-fixed');\n```\n"
	fixResp := autoFixResponseMsg{
		content:  fixContent,
		provider: "codex",
		cost:     0.05,
		attempt:  1,
	}
	m, cmd := app.Update(fixResp)
	app = m.(App)

	if app.autoFixRetryAttempt != 1 {
		t.Errorf("autoFixRetryAttempt = %d, want 1", app.autoFixRetryAttempt)
	}
	if cmd == nil {
		t.Fatal("expected filesExtractedMsg command for fix files")
	}
	msg := cmd()
	extracted, ok := msg.(filesExtractedMsg)
	if !ok {
		t.Fatalf("command produced %T, want filesExtractedMsg", msg)
	}
	if extracted.phase != pendingPhaseFix {
		t.Errorf("phase = %v, want pendingPhaseFix", extracted.phase)
	}
}

func TestMode_AutoFixCycle_FixFilesRequireConfirmation(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)

	// Send fix-phase extracted files → should trigger confirmation prompt.
	app = driveMessages(t, app, filesExtractedMsg{
		files: []engine.ExtractedFile{{Path: "fix.js", Content: "fixed code"}},
		phase: pendingPhaseFix,
	})

	if app.state != StateConfirmFiles {
		t.Error("state should be StateConfirmFiles for auto-fix files")
	}
	if app.pendingPhase != pendingPhaseFix {
		t.Errorf("pendingPhase = %v, want pendingPhaseFix", app.pendingPhase)
	}
}

func TestMode_AutoFixCycle_MaxRetriesExhausted(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)

	// Simulate max+1 attempt.
	fix := autoFixMsg{errOutput: "test error", attempt: maxAutoFixRetries + 1}
	app = driveMessages(t, app, fix)

	if app.wizard.Phase() != WizardPhaseDone {
		t.Errorf("wizard phase = %v, want WizardPhaseDone after max retries", app.wizard.Phase())
	}

	// Should contain max retries message.
	found := false
	for _, m := range app.chat.messages {
		if strings.Contains(m.Content, fmt.Sprintf("%d", maxAutoFixRetries)) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected max retries exhausted message in chat")
	}
}

// ---------------------------------------------------------------------------
// Chat Mode Tests
// ---------------------------------------------------------------------------

func TestMode_ChatStreaming_FullFlow(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")

	// Simulate stream start.
	ch := make(chan model.StreamChunk, 3)
	ch <- model.StreamChunk{Content: "Hello "}
	ch <- model.StreamChunk{Content: "world"}
	ch <- model.StreamChunk{Done: true}
	close(ch)

	start := aiStreamStartMsg{ch: ch, provider: "claude"}
	app = driveMessages(t, app, start)

	if app.streamCh == nil {
		t.Fatal("streamCh should be set after aiStreamStartMsg")
	}

	// Process chunks.
	app = driveMessages(t, app,
		aiStreamMsg{chunk: model.StreamChunk{Content: "Hello "}, provider: "claude"},
		aiStreamMsg{chunk: model.StreamChunk{Content: "world"}, provider: "claude"},
		aiStreamMsg{chunk: model.StreamChunk{Done: true}, provider: "claude"},
	)

	if app.chat.streaming {
		t.Error("streaming should be false after Done chunk")
	}
	if app.streamCh != nil {
		t.Error("streamCh should be nil after Done chunk")
	}
	// Verify message was added.
	found := false
	for _, m := range app.chat.messages {
		if m.Role == "assistant" && strings.Contains(m.Content, "Hello world") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected assistant message containing 'Hello world'")
	}
}

func TestMode_ChatStreaming_FallbackNotice(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")

	start := aiStreamStartMsg{
		ch:         make(chan model.StreamChunk),
		provider:   "gemini",
		isFallback: true,
		requested:  "claude",
	}
	app = driveMessages(t, app, start)

	// Should have a fallback notice system message.
	found := false
	for _, m := range app.chat.messages {
		if m.Role == "system" && strings.Contains(m.Content, "claude") && strings.Contains(m.Content, "gemini") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected fallback notice mentioning both provider names")
	}
}

func TestMode_ChatStreaming_ErrorMidStream(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")

	ch := make(chan model.StreamChunk, 1)
	close(ch)
	app.streamCh = ch
	app.chat.SetStreaming(true)

	// Error chunk.
	errChunk := aiStreamMsg{
		chunk:    model.StreamChunk{Error: fmt.Errorf("connection reset")},
		provider: "claude",
	}
	app = driveMessages(t, app, errChunk)

	if app.chat.streaming {
		t.Error("streaming should be false after error")
	}

	found := false
	for _, m := range app.chat.messages {
		if m.Role == "system" && strings.Contains(m.Content, "connection reset") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected error message in chat")
	}
}

func TestMode_ChatStreaming_DuplicateGuard(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")
	app.chat.streaming = true
	app.streamCh = make(chan model.StreamChunk) // non-nil → streaming active

	// handleChatEnter should return nil cmd (no duplicate).
	m, cmd := app.handleChatEnter()
	app = m.(App)
	if cmd != nil {
		t.Error("handleChatEnter should return nil cmd when already streaming")
	}
}

func TestMode_ChatConversationSummary(t *testing.T) {
	chat := NewChatPanel()
	for i := 1; i <= 25; i++ {
		chat.AddMessage(ChatMessage{Role: "user", Content: fmt.Sprintf("msg-%02d", i)})
		chat.AddMessage(ChatMessage{Role: "assistant", Content: fmt.Sprintf("reply-%02d", i)})
	}

	msgs := chat.ToModelMessages()
	if len(msgs) > maxChatHistory {
		t.Fatalf("ToModelMessages returned %d, want <= %d", len(msgs), maxChatHistory)
	}
	if msgs[0].Role != "system" {
		t.Fatalf("first message role = %q, want system (summary)", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "summary") {
		t.Errorf("first message should be a conversation summary, got %q", msgs[0].Content[:80])
	}
}

// ---------------------------------------------------------------------------
// Mode Switch Tests
// ---------------------------------------------------------------------------

func TestMode_SwitchCommand_FreeToBalanced(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")
	app.router.SetMode(model.ModeFree)

	m, _ := app.handleModeCommand("/mode balanced")
	app = m.(App)

	if app.router.Mode() != model.ModeBalanced {
		t.Errorf("router.Mode() = %v, want ModeBalanced", app.router.Mode())
	}
}

func TestMode_SwitchCommand_ShowCurrent(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")
	app.router.SetMode(model.ModeEconomy)

	m, _ := app.handleModeCommand("/mode")
	app = m.(App)

	lastMsg := app.chat.messages[len(app.chat.messages)-1]
	if !strings.Contains(lastMsg.Content, "economy") {
		t.Errorf("expected current mode in message, got %q", lastMsg.Content)
	}
}

func TestMode_SwitchCommand_InvalidMode(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")
	app.router.SetMode(model.ModeBalanced)

	m, _ := app.handleModeCommand("/mode invalid")
	app = m.(App)

	// Mode should be unchanged.
	if app.router.Mode() != model.ModeBalanced {
		t.Errorf("mode changed to %v after invalid input, want ModeBalanced", app.router.Mode())
	}

	// Should show help text.
	lastMsg := app.chat.messages[len(app.chat.messages)-1]
	if !strings.Contains(lastMsg.Content, "free|economy|balanced|power") {
		t.Errorf("expected help text in message, got %q", lastMsg.Content)
	}
}

func TestMode_SwitchCommand_NoPanicNarrowWindow(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")
	m, _ := app.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	app = m.(App)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleModeCommand panicked on narrow window with power mode: %v", r)
		}
	}()

	_, _ = app.handleModeCommand("/mode power")
}

func TestMode_FileWriteRefreshFailureSurfacesSystemMessage(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)
	app.pendingPhase = pendingPhaseBuild

	if err := os.RemoveAll(app.project.Path); err != nil {
		t.Fatalf("RemoveAll(project.Path): %v", err)
	}

	m, _ := app.handleFileWriteComplete(fileWriteCompleteMsg{written: 1})
	app = m.(App)

	if !chatContains(app.chat.messages, "Failed to refresh project files:") {
		t.Fatalf("expected refresh failure message, got %#v", app.chat.messages)
	}
}

func TestMode_FilesUpdatedRefreshFailureSurfacesSystemMessage(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)

	if err := os.RemoveAll(app.project.Path); err != nil {
		t.Fatalf("RemoveAll(project.Path): %v", err)
	}

	m, _ := app.Update(filesUpdatedMsg{})
	app = m.(App)

	if !chatContains(app.chat.messages, "Failed to refresh project files:") {
		t.Fatalf("expected refresh failure message, got %#v", app.chat.messages)
	}
}

func chatContains(messages []ChatMessage, needle string) bool {
	for _, msg := range messages {
		if strings.Contains(msg.Content, needle) {
			return true
		}
	}
	return false
}
