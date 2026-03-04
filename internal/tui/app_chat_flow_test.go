package tui

import (
	"context"
	"fmt"
	"testing"

	"github.com/makewand/makewand/internal/config"
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
