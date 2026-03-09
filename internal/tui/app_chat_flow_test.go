package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

type chatFlowStubProvider struct {
	name string

	chatContent string
	chatUsage   model.Usage
	chatErr     error

	streamErr error

	chatCalls   int
	streamCalls int
}

func (p *chatFlowStubProvider) Name() string { return p.name }

func (p *chatFlowStubProvider) IsAvailable() bool { return true }

func (p *chatFlowStubProvider) Chat(context.Context, []model.Message, string, int) (string, model.Usage, error) {
	p.chatCalls++
	if p.chatErr != nil {
		return "", model.Usage{}, p.chatErr
	}
	usage := p.chatUsage
	if usage.Provider == "" {
		usage.Provider = p.name
	}
	return p.chatContent, usage, nil
}

func (p *chatFlowStubProvider) ChatStream(context.Context, []model.Message, string, int) (<-chan model.StreamChunk, error) {
	p.streamCalls++
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	ch := make(chan model.StreamChunk, 2)
	ch <- model.StreamChunk{Content: "stream ok"}
	ch <- model.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func TestSubmitChatInput_PowerModeUsesChatBestPath(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	router := model.NewRouter(cfg)
	router.SetMode(model.ModePower)
	stub := &chatFlowStubProvider{
		name:        "private",
		chatContent: "power answer",
		chatUsage: model.Usage{
			Provider:     "private",
			InputTokens:  12,
			OutputTokens: 34,
			Cost:         0.42,
		},
		// If ChatStream gets called here, this test should fail.
		streamErr: fmt.Errorf("stream should not be used in power mode"),
	}
	if err := router.RegisterProvider("private", stub, model.AccessSubscription); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	app.router = router

	m, cmd := app.submitChatInput("build a small cli todo app")
	app = m.(App)
	if cmd == nil {
		t.Fatal("submitChatInput returned nil cmd")
	}

	msg := cmd()
	resp, ok := msg.(aiResponseMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want aiResponseMsg", msg)
	}
	if resp.err != nil {
		t.Fatalf("aiResponseMsg.err = %v", resp.err)
	}
	if resp.content != "power answer" {
		t.Fatalf("aiResponseMsg.content = %q, want %q", resp.content, "power answer")
	}
	if resp.provider != "private" {
		t.Fatalf("aiResponseMsg.provider = %q, want %q", resp.provider, "private")
	}
	if stub.chatCalls != 1 {
		t.Fatalf("Chat calls = %d, want 1", stub.chatCalls)
	}
	if stub.streamCalls != 0 {
		t.Fatalf("ChatStream calls = %d, want 0", stub.streamCalls)
	}

	m, _ = app.Update(resp)
	app = m.(App)
	if app.chat.streaming {
		t.Fatal("chat.streaming should be false after aiResponseMsg")
	}
}

func TestSubmitChatInput_NonPowerUsesStreamPath(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	router := model.NewRouter(cfg)
	router.SetMode(model.ModeBalanced)
	stub := &chatFlowStubProvider{
		name:    "private",
		chatErr: fmt.Errorf("chat should not be used in non-power stream path"),
	}
	if err := router.RegisterProvider("private", stub, model.AccessSubscription); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	app.router = router

	m, cmd := app.submitChatInput("write a tiny function")
	_ = m.(App)
	if cmd == nil {
		t.Fatal("submitChatInput returned nil cmd")
	}

	msg := cmd()
	start, ok := msg.(aiStreamStartMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want aiStreamStartMsg", msg)
	}
	if start.provider != "private" {
		t.Fatalf("aiStreamStartMsg.provider = %q, want %q", start.provider, "private")
	}
	if stub.streamCalls != 1 {
		t.Fatalf("ChatStream calls = %d, want 1", stub.streamCalls)
	}
	if stub.chatCalls != 0 {
		t.Fatalf("Chat calls = %d, want 0", stub.chatCalls)
	}
}

func TestSubmitChatInput_ExplainUsesUnaryChatPath(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	router := model.NewRouter(cfg)
	router.SetMode(model.ModeBalanced)
	stub := &chatFlowStubProvider{
		name:        "private",
		chatContent: "I am makewand",
		chatUsage: model.Usage{
			Provider: "private",
		},
		streamErr: fmt.Errorf("stream should not be used for explain tasks"),
	}
	if err := router.RegisterProvider("private", stub, model.AccessSubscription); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	app.router = router

	m, cmd := app.submitChatInput("你是谁?")
	_ = m.(App)
	if cmd == nil {
		t.Fatal("submitChatInput returned nil cmd")
	}

	msg := cmd()
	resp, ok := msg.(aiResponseMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want aiResponseMsg", msg)
	}
	if resp.err != nil {
		t.Fatalf("aiResponseMsg.err = %v", resp.err)
	}
	if stub.chatCalls != 1 {
		t.Fatalf("Chat calls = %d, want 1", stub.chatCalls)
	}
	if stub.streamCalls != 0 {
		t.Fatalf("ChatStream calls = %d, want 0", stub.streamCalls)
	}
}

func TestClassifyTask_UsesSharedClassifier(t *testing.T) {
	prompts := []string{
		"please review.",
		"fix, please",
		"bug?",
		"checkout the repo",
		"how does this work",
		"plan this feature",
	}

	for _, prompt := range prompts {
		got := classifyTask(prompt)
		want := model.ClassifyTask(prompt)
		if got != want {
			t.Fatalf("classifyTask(%q) = %v, want shared classifier result %v", prompt, got, want)
		}
	}
}

func TestSubmitChatInput_HelpCommandHandledLocally(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	before := len(app.chat.messages)
	m, cmd := app.submitChatInput("/help")
	app = m.(App)

	if cmd != nil {
		t.Fatal("/help should not trigger async AI command")
	}
	if len(app.chat.messages) != before+1 {
		t.Fatalf("chat message count = %d, want %d", len(app.chat.messages), before+1)
	}

	last := app.chat.messages[len(app.chat.messages)-1]
	if last.Role != "system" {
		t.Fatalf("last message role = %q, want system", last.Role)
	}
	if !strings.Contains(last.Content, "/model") || !strings.Contains(last.Content, "/exit") || !strings.Contains(last.Content, "/clear") {
		t.Fatalf("help message = %q, want slash command hint", last.Content)
	}
}

func TestSubmitChatInput_ExitCommandQuits(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	m, cmd := app.submitChatInput("/exit")
	app = m.(App)

	if app.state != StateQuitting {
		t.Fatal("app should enter quitting state on /exit")
	}
	if cmd == nil {
		t.Fatal("/exit should return tea.Quit command")
	}
	quitMsg := cmd()
	if _, ok := quitMsg.(tea.QuitMsg); !ok {
		t.Fatalf("/exit cmd() should return tea.QuitMsg, got %T", quitMsg)
	}
}

func TestSubmitChatInput_ClearCommandResetsConversation(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	app.chat.AddMessage(ChatMessage{Role: "user", Content: "before clear"})

	m, cmd := app.submitChatInput("/clear")
	app = m.(App)

	if cmd == nil {
		t.Fatal("/clear should return a clear-screen command")
	}
	if len(app.chat.messages) != 1 {
		t.Fatalf("chat message count = %d, want 1 welcome message", len(app.chat.messages))
	}
	if app.chat.messages[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", app.chat.messages[0].Role)
	}
}

func TestSubmitChatInput_ModelAliasSwitchesMode(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	app.router.SetMode(model.ModeBalanced)

	m, cmd := app.submitChatInput("/model power")
	app = m.(App)

	if cmd != nil {
		t.Fatal("/model should be handled locally")
	}
	if got := app.router.Mode(); got != model.ModePower {
		t.Fatalf("router mode = %v, want %v", got, model.ModePower)
	}
}

func TestSubmitChatInput_StatusCommandHandledLocally(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	m, cmd := app.submitChatInput("/status")
	app = m.(App)

	if cmd != nil {
		t.Fatal("/status should not trigger async AI command")
	}
	last := app.chat.messages[len(app.chat.messages)-1]
	if !strings.Contains(last.Content, "Model profile:") || !strings.Contains(last.Content, "Available providers:") {
		t.Fatalf("status message = %q, want session summary", last.Content)
	}
}

func TestSubmitChatInput_CostCommandHandledLocally(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	app.cost.AddWithTokens("claude", 0.25, 100, 200, true)

	m, cmd := app.submitChatInput("/cost")
	app = m.(App)

	if cmd != nil {
		t.Fatal("/cost should not trigger async AI command")
	}
	last := app.chat.messages[len(app.chat.messages)-1]
	if !strings.Contains(last.Content, "Session total: $0.25") || !strings.Contains(last.Content, "claude:") {
		t.Fatalf("cost message = %q, want cost summary", last.Content)
	}
}

func TestHandleChatEnter_AppliesSelectedSlashSuggestionBeforeExecuting(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	app.chat, _ = app.chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	app.chat.textarea.SetValue("/")
	app.chat, _ = app.chat.Update(tea.KeyMsg{Type: tea.KeyDown})

	before := len(app.chat.messages)
	m, cmd := app.handleChatEnter()
	app = m.(App)

	if cmd != nil {
		t.Fatal("selecting a slash suggestion should not execute a command yet")
	}
	if got := app.chat.InputValue(); got != "/clear" {
		t.Fatalf("input after Enter = %q, want %q", got, "/clear")
	}
	if len(app.chat.messages) != before {
		t.Fatalf("chat message count = %d, want unchanged %d", len(app.chat.messages), before)
	}
}

func TestSubmitChatInput_ApproveCommandHandlesPendingTests(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	app.state = StateConfirmTests
	app.setPendingApproval(approvalTests, "Run project tests now?", "Command: go test ./...")

	m, cmd := app.submitChatInput("/approve")
	app = m.(App)

	if cmd == nil {
		t.Fatal("/approve should return a confirmation command when approval is pending")
	}
	if app.state == StateConfirmTests {
		t.Fatal("state should not be StateConfirmTests after /approve")
	}
	msg := cmd()
	if _, ok := msg.(confirmTestsRunMsg); !ok {
		t.Fatalf("/approve returned %T, want confirmTestsRunMsg", msg)
	}
}

func TestSubmitChatInput_DenyCommandCancelsPendingFileWrite(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	app.state = StateConfirmFiles
	app.pendingFiles = []engine.ExtractedFile{{Path: "main.go", Content: "package main"}}
	app.setPendingApproval(approvalFileWrite, "Write 1 file?", "Pending write: 1 files")

	m, cmd := app.submitChatInput("/deny")
	app = m.(App)

	if cmd != nil {
		t.Fatal("/deny for file write should complete locally")
	}
	if app.state == StateConfirmFiles {
		t.Fatal("state should not be StateConfirmFiles after /deny")
	}
	if len(app.pendingFiles) != 0 {
		t.Fatal("pendingFiles should be cleared after /deny")
	}
	last := app.chat.messages[len(app.chat.messages)-1]
	if !strings.Contains(last.Content, "cancel") && !strings.Contains(strings.ToLower(last.Content), "cancel") {
		t.Fatalf("deny message = %q, want cancellation notice", last.Content)
	}
}
