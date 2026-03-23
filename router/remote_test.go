package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoteHTTPProvider_Chat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q, want Bearer secret", got)
		}
		var req httpChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.Messages) != 2 {
			t.Fatalf("messages = %+v, want system+user", req.Messages)
		}
		if req.Mode != "power" {
			t.Fatalf("mode = %q, want power", req.Mode)
		}
		resp := httpChatResponse{
			Model: "remote-model",
			Choices: []httpChoice{{
				Message:      httpMessage{Role: "assistant", Content: "hello from remote"},
				FinishReason: "stop",
			}},
			Usage: httpUsageResponse{
				PromptTokens:     7,
				CompletionTokens: 5,
				TotalTokens:      12,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewRemoteHTTP(server.URL, "secret")
	provider.chatClient = server.Client()

	ctx := ContextWithUsageMode(context.Background(), ModePower)
	got, usage, err := provider.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, "system prompt", 1024)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got != "hello from remote" {
		t.Fatalf("content = %q, want hello from remote", got)
	}
	if usage.Model != "remote-model" {
		t.Fatalf("usage.Model = %q, want remote-model", usage.Model)
	}
	if usage.Provider != "remote" {
		t.Fatalf("usage.Provider = %q, want remote", usage.Provider)
	}
}

func TestRemoteHTTPProvider_ChatStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q, want Bearer secret", got)
		}
		var req httpChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !req.Stream {
			t.Fatal("req.Stream = false, want true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"mw-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"remote-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"mw-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"remote-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"stream\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"mw-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"remote-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := NewRemoteHTTP(server.URL, "secret")
	provider.chatClient = server.Client()

	ch, err := provider.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var content string
	done := false
	for chunk := range ch {
		content += chunk.Content
		if chunk.Done {
			done = true
		}
		if chunk.Error != nil {
			t.Fatalf("stream chunk error: %v", chunk.Error)
		}
	}

	if content != "hello stream" {
		t.Fatalf("streamed content = %q, want hello stream", content)
	}
	if !done {
		t.Fatal("stream did not report Done")
	}
}

func TestRemoteHTTPProvider_ChatStream_LegacyJSONFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := httpChatResponse{
			Model: "remote-model",
			Choices: []httpChoice{{
				Message:      httpMessage{Role: "assistant", Content: "hello stream"},
				FinishReason: "stop",
			}},
			Usage: httpUsageResponse{},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewRemoteHTTP(server.URL, "secret")
	provider.chatClient = server.Client()

	ch, err := provider.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var content string
	done := false
	for chunk := range ch {
		content += chunk.Content
		if chunk.Done {
			done = true
		}
		if chunk.Error != nil {
			t.Fatalf("stream chunk error: %v", chunk.Error)
		}
	}

	if content != "hello stream" {
		t.Fatalf("streamed content = %q, want hello stream", content)
	}
	if !done {
		t.Fatal("stream did not report Done")
	}
}

func TestRemoteHTTPProvider_ChatStream_SurfacesRemoteStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"stream exploded\"}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := NewRemoteHTTP(server.URL, "secret")
	provider.chatClient = server.Client()

	ch, err := provider.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 1024)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	foundErr := false
	for chunk := range ch {
		if chunk.Error != nil {
			foundErr = true
			if !strings.Contains(chunk.Error.Error(), "stream exploded") {
				t.Fatalf("chunk.Error = %v, want stream exploded", chunk.Error)
			}
		}
	}
	if !foundErr {
		t.Fatal("stream error was not surfaced")
	}
}
