package tui

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/remotesession"
)

func TestChatSessionSaveAndRestore_RemoteBackendAcrossPaths(t *testing.T) {
	server := httptest.NewServer(remotesession.NewHandler(remotesession.NewStore(t.TempDir()), "secret"))
	defer server.Close()

	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Setenv("MAKEWAND_REMOTE_URL", server.URL)
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "secret")
	t.Setenv("MAKEWAND_WORKSPACE_ID", "shared-workspace")

	projectA, err := engine.NewProject("session-remote-a", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject(A): %v", err)
	}
	projectB, err := engine.NewProject("session-remote-b", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject(B): %v", err)
	}

	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, projectA.Path)
	app.chat.AddMessage(ChatMessage{Role: "user", Content: "resume me"})
	app.chat.AddMessage(ChatMessage{Role: "assistant", Content: "shared session"})

	if err := app.saveChatSession(); err != nil {
		t.Fatalf("saveChatSession: %v", err)
	}
	if app.sessionFile != "remote://shared-workspace" {
		t.Fatalf("sessionFile = %q, want remote://shared-workspace", app.sessionFile)
	}

	restored := *NewApp(ModeChat, cfg, projectB.Path)
	ok, err := restored.restoreChatSession()
	if err != nil {
		t.Fatalf("restoreChatSession: %v", err)
	}
	if !ok {
		t.Fatal("restoreChatSession should report a restored remote session")
	}
	if !strings.Contains(restored.chat.LastAssistantContent(), "shared session") {
		t.Fatalf("restored assistant content = %q, want shared session", restored.chat.LastAssistantContent())
	}
}
