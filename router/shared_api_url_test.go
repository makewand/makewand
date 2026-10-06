package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConfiguredAPIEndpointHandlesChatAndStream(t *testing.T) {
	for _, name := range []string{"claude", "openai", "gemini"} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				stream, _ := payload["stream"].(bool)
				switch name {
				case "claude":
					if r.URL.Path != "/gateway/v1/messages" || r.Header.Get("x-api-key") != "fixture-key" || payload["model"] != "fixture-model" {
						t.Errorf("unexpected Claude request: %s %+v", r.URL, payload)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}]}`)
					}
				case "openai":
					if r.URL.Path != "/gateway/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" || payload["model"] != "fixture-model" {
						t.Errorf("unexpected OpenAI request: %s %+v", r.URL, payload)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
					}
				case "gemini":
					stream = strings.Contains(r.URL.Path, "streamGenerateContent")
					suffix := "generateContent"
					if stream {
						suffix = "streamGenerateContent"
					}
					if r.URL.Path != "/gateway/v1beta/models/fixture-model:"+suffix || r.Header.Get("x-goog-api-key") != "fixture-key" {
						t.Errorf("unexpected Gemini request: %s", r.URL)
					}
					response := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\n", response)
					} else {
						fmt.Fprint(w, response)
					}
				}
			}))
			defer server.Close()
			base := server.URL + "/gateway"
			var provider Provider
			switch name {
			case "claude":
				provider = NewClaudeWithBaseURL("fixture-key", "fixture-model", base+"/")
			case "openai":
				provider = NewOpenAIWithBaseURL("fixture-key", "fixture-model", base+"/v1/")
			case "gemini":
				provider = NewGeminiWithBaseURL("fixture-key", "fixture-model", base+"/")
			}
			content, _, err := provider.Chat(context.Background(), []Message{{Role: "user", Content: "fixture"}}, "", 128)
			if err != nil || content != "ok" {
				t.Fatalf("Chat: %q %v", content, err)
			}
			chunks, err := provider.ChatStream(context.Background(), []Message{{Role: "user", Content: "fixture"}}, "", 128)
			if err != nil {
				t.Fatal(err)
			}
			var result strings.Builder
			for chunk := range chunks {
				if chunk.Error != nil {
					t.Fatal(chunk.Error)
				}
				result.WriteString(chunk.Content)
			}
			if result.String() != "ok" || requests.Load() != 2 {
				t.Fatalf("stream=%q requests=%d", result.String(), requests.Load())
			}
		})
	}
}
