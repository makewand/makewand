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

func TestChatPanel_MouseWheelScrollAndPreserveOffset(t *testing.T) {
	chat := NewChatPanel()
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Add enough messages so viewport has scrollable content
	for i := 0; i < 30; i++ {
		chat.AddMessage(ChatMessage{Role: "user", Content: "This is test message long enough to wrap and fill line"})
	}

	// Viewport starts at bottom (YOffset > 0)
	initialOffset := chat.viewport.YOffset
	if initialOffset == 0 {
		t.Fatalf("expected initial YOffset > 0 with 30 messages, got %d", initialOffset)
	}

	// Scroll up using mouse wheel
	chat, _ = chat.Update(tea.MouseMsg{Type: tea.MouseWheelUp})
	if chat.viewport.YOffset >= initialOffset {
		t.Fatalf("expected YOffset to decrease after MouseWheelUp, got %d (initial %d)", chat.viewport.YOffset, initialOffset)
	}
	scrolledOffset := chat.viewport.YOffset

	// Vertical scroll keys (b, u, k, j, space, f) must NOT cause viewport to scroll while typing!
	for _, key := range []rune{'b', 'u', 'k', 'j', 'f', ' '} {
		chat, _ = chat.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		if chat.viewport.YOffset != scrolledOffset {
			t.Fatalf("YOffset changed from %d to %d after typing vertical scroll key %q; keystrokes must not leak into viewport", scrolledOffset, chat.viewport.YOffset, key)
		}
	}
}

func TestChatPanel_WindowResizePreservesBottomFollow(t *testing.T) {
	chat := NewChatPanel()
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	for i := 0; i < 20; i++ {
		chat.AddMessage(ChatMessage{Role: "user", Content: "Line of message content to fill the viewport"})
	}

	if !chat.viewport.AtBottom() {
		t.Fatal("expected viewport to be at bottom initially")
	}

	// Shrink window height
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 18})
	if !chat.viewport.AtBottom() {
		t.Fatal("expected viewport to remain at bottom after shrinking window")
	}

	// Expand window height
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if !chat.viewport.AtBottom() {
		t.Fatal("expected viewport to remain at bottom after expanding window")
	}
}

