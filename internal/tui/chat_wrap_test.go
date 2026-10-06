package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/makewand/makewand/internal/i18n"
)

// Read the actual viewport, including every scrollable line, so this checks
// clipping as well as the text supplied to SetContent.
func assertChatViewportText(t *testing.T, chat ChatPanel, want string) {
	t.Helper()
	chat.viewport.Height = chat.viewport.TotalLineCount()
	chat.viewport.GotoTop()
	visible := ansi.Strip(chat.viewport.View())
	for i, line := range strings.Split(visible, "\n") {
		if width := ansi.StringWidth(line); width > chat.viewport.Width {
			t.Fatalf("viewport line %d has width %d, available %d: %q", i, width, chat.viewport.Width, line)
		}
	}
	for i, line := range strings.Split(chat.renderMessages(), "\n") {
		if width := ansi.StringWidth(line); width > maxInt(chat.width-4, minViewportWidth) {
			t.Fatalf("message line %d has width %d, available %d: %q", i, width, maxInt(chat.width-4, minViewportWidth), line)
		}
	}
	compact := func(s string) string { return strings.Join(strings.Fields(s), "") }
	if got := compact(visible); got != compact(want) {
		t.Fatalf("viewport lost or changed text\ngot:  %q\nwant: %q", got, compact(want))
	}
}

func TestChatPanel_WelcomeAndHelpRemainCompleteAfterResize(t *testing.T) {
	previousLanguage := i18n.GetLanguage()
	t.Cleanup(func() { i18n.SetLanguage(previousLanguage) })
	for _, language := range []string{"en", "zh"} {
		t.Run(language, func(t *testing.T) {
			i18n.SetLanguage(language)
			app := App{}
			for _, message := range []ChatMessage{app.chatWelcomeMessage(), {Role: "system", Content: app.chatHelpText()}} {
				chat := NewChatPanel()
				chat.AddMessage(message)
				for _, width := range []int{120, 44, 18, 8, 120} {
					chat, _ = chat.Update(tea.WindowSizeMsg{Width: width, Height: 24})
					assertChatViewportText(t, chat, "--- "+message.Content+" ---")
				}
			}
		})
	}
}

func TestChatPanel_StatusAndLongUnicodeRemainCompleteAfterResize(t *testing.T) {
	status := "Review waiting for approval\n状态：准备执行离线检查并保留用户更改 🧑‍💻 e\u0301\n" + strings.Repeat("长路径", 14) + "/" + strings.Repeat("unbroken", 14)
	provider := "offline-" + strings.Repeat("provider", 8)
	chat := NewChatPanel()
	chat.AddMessage(ChatMessage{Role: "status", Content: status})
	chat.AddMessage(ChatMessage{Role: "assistant", Provider: provider, Content: "中文连续文本没有空格需要按显示宽度换行 🧑‍💻 e\u0301", Cost: 0.0123})
	want := "* " + status + "\nAI (" + provider + ")\n" + chat.messages[1].Content + "\n  $0.0123"
	for _, width := range []int{120, 44, 18, 8, 120} {
		chat, _ = chat.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		assertChatViewportText(t, chat, want)
	}
}

func TestChatPanel_StreamingStatusIncludesPrefixInWrapWidth(t *testing.T) {
	for _, provider := range []string{"", "offline-" + strings.Repeat("provider", 8)} {
		t.Run(fmt.Sprintf("provider=%t", provider != ""), func(t *testing.T) {
			chat := NewChatPanel()
			chat.SetStreaming(true)
			chat.SetStreamProvider(provider)
			chat.SetStreamStatus("等待处理：" + strings.Repeat("进度", 12) + " 🧑‍💻 e\u0301")
			want := "* " + chat.streamStatus
			if provider != "" {
				want = "AI (" + provider + ") *\n" + chat.streamStatus
			}
			for _, width := range []int{80, 18, 8, 80} {
				chat, _ = chat.Update(tea.WindowSizeMsg{Width: width, Height: 24})
				assertChatViewportText(t, chat, want)
			}
		})
	}
}
