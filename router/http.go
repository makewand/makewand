// http.go — OpenAI-compatible HTTP facade for the Router.
//
// Usage:
//
//	r, _ := router.NewRouterFromConfig(rc)
//	http.ListenAndServe(":8080", r.HTTPHandler())
//
// This exposes POST /v1/chat/completions with the standard OpenAI request/response schema,
// routing through the Router's provider selection logic.
package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// httpChatRequest is the subset of the OpenAI chat completions request we support.
type httpChatRequest struct {
	Model       string        `json:"model"`
	Messages    []httpMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

type httpMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// httpChatResponse mirrors the OpenAI chat completions response.
type httpChatResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []httpChoice     `json:"choices"`
	Usage   httpUsageResponse `json:"usage"`
}

type httpChoice struct {
	Index        int         `json:"index"`
	Message      httpMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type httpUsageResponse struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type httpErrorResponse struct {
	Error httpErrorBody `json:"error"`
}

type httpErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// HTTPHandler returns an http.Handler that serves an OpenAI-compatible
// /v1/chat/completions endpoint backed by this Router.
//
// Supported features:
//   - POST /v1/chat/completions (non-streaming)
//   - Automatic task classification from message content
//   - Provider routing via the Router's strategy tables
//   - GET /v1/models lists available providers
func (r *Router) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", r.handleChatCompletions)
	mux.HandleFunc("/v1/models", r.handleListModels)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return mux
}

func (r *Router) handleChatCompletions(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}

	var chatReq httpChatRequest
	if err := json.NewDecoder(req.Body).Decode(&chatReq); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}

	if chatReq.Stream {
		writeHTTPError(w, http.StatusBadRequest, "unsupported", "streaming is not yet supported via HTTP; use the Go API directly")
		return
	}

	if len(chatReq.Messages) == 0 {
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", "messages array is empty")
		return
	}

	// Convert messages
	messages := make([]Message, 0, len(chatReq.Messages))
	var system string
	for _, m := range chatReq.Messages {
		if m.Role == "system" {
			system = m.Content
			continue
		}
		messages = append(messages, Message{Role: m.Role, Content: m.Content})
	}

	// Classify task from the last user message
	task := TaskCode
	for i := len(chatReq.Messages) - 1; i >= 0; i-- {
		if chatReq.Messages[i].Role == "user" {
			task = ClassifyTask(chatReq.Messages[i].Content)
			break
		}
	}

	ctx := req.Context()
	content, usage, result, err := r.Chat(ctx, task, messages, system)
	if err != nil {
		status := http.StatusServiceUnavailable
		if strings.Contains(err.Error(), "not configured") {
			status = http.StatusBadRequest
		}
		writeHTTPError(w, status, "provider_error", err.Error())
		return
	}

	resp := httpChatResponse{
		ID:      fmt.Sprintf("mw-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   result.ModelID,
		Choices: []httpChoice{
			{
				Index:        0,
				Message:      httpMessage{Role: "assistant", Content: content},
				FinishReason: "stop",
			},
		},
		Usage: httpUsageResponse{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			TotalTokens:      usage.InputTokens + usage.OutputTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (r *Router) handleListModels(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	type modelsResponse struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	avail := r.Available()
	data := make([]modelEntry, 0, len(avail))
	for _, name := range avail {
		data = append(data, modelEntry{
			ID:      name,
			Object:  "model",
			OwnedBy: "makewand",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelsResponse{Object: "list", Data: data})
}

func writeHTTPError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(httpErrorResponse{
		Error: httpErrorBody{
			Message: message,
			Type:    "error",
			Code:    code,
		},
	})
}
