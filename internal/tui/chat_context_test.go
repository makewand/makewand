package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestToModelMessages_SummarizesOlderContext(t *testing.T) {
	chat := NewChatPanel()
	for i := 1; i <= 25; i++ {
		chat.AddMessage(ChatMessage{Role: "user", Content: fmt.Sprintf("user-message-%02d", i)})
	}

	msgs := chat.ToModelMessages()
	if len(msgs) != maxChatHistory {
		t.Fatalf("ToModelMessages len=%d, want %d", len(msgs), maxChatHistory)
	}
	if msgs[0].Role != "system" {
		t.Fatalf("first message role=%q, want %q", msgs[0].Role, "system")
	}
	if !strings.Contains(msgs[0].Content, "Conversation summary of earlier context") {
		t.Fatalf("summary header missing: %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "user-message-01") {
		t.Fatalf("summary should contain early message context: %q", msgs[0].Content)
	}
	if msgs[len(msgs)-1].Content != "user-message-25" {
		t.Fatalf("last message=%q, want latest user message", msgs[len(msgs)-1].Content)
	}
}

func TestToModelMessages_NoSummaryForShortHistory(t *testing.T) {
	chat := NewChatPanel()
	for i := 1; i <= 5; i++ {
		chat.AddMessage(ChatMessage{Role: "user", Content: fmt.Sprintf("short-%d", i)})
	}

	msgs := chat.ToModelMessages()
	if len(msgs) != 5 {
		t.Fatalf("ToModelMessages len=%d, want 5", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("first message role=%q, want %q", msgs[0].Role, "user")
	}
}
