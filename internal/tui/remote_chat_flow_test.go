package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/makewand/makewand/internal/config"
)

func TestSubmitChatInput_RemoteOnlyExplainUsesUnaryChatPath(t *testing.T) {
	type remoteRequest struct {
		Mode string `json:"mode"`
	}
	type remoteResponse struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q, want Bearer secret", got)
		}

		var req remoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request): %v", err)
		}
		if req.Mode != "balanced" {
			t.Fatalf("req.Mode = %q, want balanced", req.Mode)
		}

		w.Header().Set("Content-Type", "application/json")
		resp := remoteResponse{Model: "remote-model"}
		resp.Choices = []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}{
			{
				Message: struct {
					Content string `json:"content"`
				}{Content: "tui remote ok"},
			},
		}
		resp.Usage.PromptTokens = 11
		resp.Usage.CompletionTokens = 3
		resp.Usage.TotalTokens = 14
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("Encode(response): %v", err)
		}
	}))
	defer server.Close()

	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Setenv("MAKEWAND_REMOTE_URL", server.URL)
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "secret")

	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")

	m, cmd := app.submitChatInput("Explain how this code works")
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
	if resp.content != "tui remote ok" {
		t.Fatalf("aiResponseMsg.content = %q, want tui remote ok", resp.content)
	}
	if resp.provider != "remote" {
		t.Fatalf("aiResponseMsg.provider = %q, want remote", resp.provider)
	}
}
