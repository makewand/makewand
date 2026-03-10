package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestChatPanel_ViewShowsSlashSuggestions(t *testing.T) {
	chat := NewChatPanel()
	chat.textarea.SetValue("/")

	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := chat.View()

	if !strings.Contains(view, "Commands (Up/Down to select, Enter/Tab to apply)") {
		t.Fatalf("view missing slash suggestion header: %q", view)
	}
	if !strings.Contains(view, "/model") || !strings.Contains(view, "/help") || !strings.Contains(view, "/clear") {
		t.Fatalf("view missing expected slash suggestions: %q", view)
	}
	if strings.Contains(view, "/model fast") {
		t.Fatalf("root command menu should not show model subcommands: %q", view)
	}
}

func TestChatPanel_TabCompletesModePrefix(t *testing.T) {
	chat := NewChatPanel()
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	chat.textarea.SetValue("/m")

	chat, _ = chat.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := chat.InputValue(); got != "/model" {
		t.Fatalf("tab completion = %q, want %q", got, "/model")
	}
}

func TestChatPanel_TabCompletesSingleSlashCommand(t *testing.T) {
	chat := NewChatPanel()
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	chat.textarea.SetValue("/model b")

	chat, _ = chat.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := chat.InputValue(); got != "/model balanced" {
		t.Fatalf("tab completion = %q, want %q", got, "/model balanced")
	}
}

func TestChatPanel_ModelMenuShowsProfiles(t *testing.T) {
	chat := NewChatPanel()
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	chat.textarea.SetValue("/model")

	view := chat.View()
	if !strings.Contains(view, "/model fast") || !strings.Contains(view, "/model power") {
		t.Fatalf("model menu missing expected profile suggestions: %q", view)
	}
	if strings.Contains(view, "/clear") {
		t.Fatalf("model submenu should not show unrelated commands: %q", view)
	}
}

func TestChatPanel_DownArrowSelectsNextSuggestion(t *testing.T) {
	chat := NewChatPanel()
	chat, _ = chat.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	chat.textarea.SetValue("/")
	chat, _ = chat.Update(tea.KeyMsg{Type: tea.KeyDown})

	if !chat.ApplySelectedSlashSuggestion() {
		t.Fatal("expected selected slash suggestion to apply")
	}
	if got := chat.InputValue(); got != "/clear" {
		t.Fatalf("selected suggestion = %q, want %q", got, "/clear")
	}
}
