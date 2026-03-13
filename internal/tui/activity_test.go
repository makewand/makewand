package tui

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/i18n"
)

func TestFormatChatActivityHeadline_Localized(t *testing.T) {
	prev := i18n.GetLanguage()
	t.Cleanup(func() {
		i18n.SetLanguage(prev)
	})

	i18n.SetLanguage("en")
	en := formatChatActivityHeadline(chatActivitySnapshot{Phase: chatActivityContext})
	if en != "Collecting project context" {
		t.Fatalf("english headline = %q", en)
	}

	i18n.SetLanguage("zh")
	zh := formatChatActivityHeadline(chatActivitySnapshot{
		Phase:    chatActivityWaiting,
		Provider: "gemini",
	})
	if !strings.Contains(zh, "正在等待 gemini 开始响应") {
		t.Fatalf("chinese headline = %q", zh)
	}
}
