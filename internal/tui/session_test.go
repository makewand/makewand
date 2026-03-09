package tui

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

func TestChatSessionSaveAndRestore(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())

	project, err := engine.NewProject("session-restore", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, project.Path)
	app.router.SetMode(model.ModePower)
	app.chat.AddMessage(ChatMessage{Role: "user", Content: "build a cli"})
	app.chat.AddMessage(ChatMessage{Role: "assistant", Content: "package main"})
	app.cost.AddWithTokens("claude", 0.42, 123, 456, true)

	if err := app.saveChatSession(); err != nil {
		t.Fatalf("saveChatSession: %v", err)
	}
	if app.sessionFile == "" {
		t.Fatal("sessionFile should be populated after save")
	}

	restored := *NewApp(ModeChat, cfg, project.Path)
	ok, err := restored.restoreChatSession()
	if err != nil {
		t.Fatalf("restoreChatSession: %v", err)
	}
	if !ok {
		t.Fatal("restoreChatSession should report a restored session")
	}
	if got := restored.router.Mode(); got != model.ModePower {
		t.Fatalf("router mode = %v, want %v", got, model.ModePower)
	}
	if got := restored.cost.SessionTotal(); got != 0.42 {
		t.Fatalf("session total = %.2f, want 0.42", got)
	}
	if !strings.Contains(restored.chat.LastAssistantContent(), "package main") {
		t.Fatalf("restored assistant content = %q, want saved reply", restored.chat.LastAssistantContent())
	}
	last := restored.chat.messages[len(restored.chat.messages)-1]
	if !strings.Contains(last.Content, restoredSessionPrefix) {
		t.Fatalf("restore notice missing, got: %q", last.Content)
	}
}

func TestMemorySummaryReportsCompactedContext(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	for i := 0; i < maxChatHistory+4; i++ {
		app.chat.AddMessage(ChatMessage{Role: "user", Content: "question"})
		app.chat.AddMessage(ChatMessage{Role: "assistant", Content: "answer"})
	}

	summary := app.memorySummary()
	if !strings.Contains(summary, "Conversation summary of earlier context:") {
		t.Fatalf("memorySummary = %q, want compacted context summary", summary)
	}
}
