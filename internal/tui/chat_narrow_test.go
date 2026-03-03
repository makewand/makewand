package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
)

func TestChatPanel_NarrowWindowDoesNotPanic(t *testing.T) {
	chat := NewChatPanel()
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 0, Height: 0})

	if chat.viewport.Width < 1 || chat.viewport.Height < 1 {
		t.Fatalf("viewport dimensions must be >= 1, got %dx%d", chat.viewport.Width, chat.viewport.Height)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AddMessage panicked on narrow window: %v", r)
		}
	}()

	chat.AddMessage(ChatMessage{Role: "system", Content: "Mode changed"})
	_ = chat.View()
}

func TestApp_HandleModeCommand_NoPanicOnNarrowWindow(t *testing.T) {
	app := NewApp(ModeChat, config.DefaultConfig(), "")
	m, _ := app.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	updated := m.(App)
	app = &updated

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleModeCommand panicked on narrow window: %v", r)
		}
	}()

	_, _ = app.handleModeCommand("/mode power")
}
