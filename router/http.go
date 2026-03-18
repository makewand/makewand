// http.go — OpenAI-compatible subset HTTP facade for the Router.
//
// Usage:
//
//	r := router.NewRouterFromConfig(rc)
//	http.ListenAndServe(":8080", r.HTTPHandler())
//
// This exposes POST /v1/chat/completions with a supported subset of the
// standard OpenAI request/response schema, routing through the Router's
// provider selection logic.
package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// httpChatRequest is the subset of the OpenAI chat completions request we support.
type httpChatRequest struct {
	Model       string        `json:"model"`
	Mode        string        `json:"mode,omitempty"`
	Messages    []httpMessage `json:"messages"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

type httpMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// httpChatResponse mirrors the OpenAI chat completions response.
type httpChatResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []httpChoice      `json:"choices"`
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

// HTTPHandlerOptions configures the HTTP facade behavior.
type HTTPHandlerOptions struct {
	// BearerToken enables authentication when non-empty.
	// Requests must include "Authorization: Bearer <token>" to proceed.
	// The /health endpoint is always unauthenticated.
	BearerToken string

	// StatsDir enables routing stats persistence after chat requests.
	// When empty, HTTP requests do not read or write routing_stats.json.
	StatsDir string
}

// HTTPHandler returns an http.Handler that serves an OpenAI-compatible subset
// /v1/chat/completions endpoint backed by this Router.
//
// Supported features:
//   - POST /v1/chat/completions (non-streaming)
//   - Automatic task classification from message content
//   - Optional provider override via the model field (provider name from /v1/models)
//   - Provider routing via the Router's strategy tables
//   - GET /v1/models lists available providers
//   - Optional Bearer token authentication
func (r *Router) HTTPHandler(opts ...HTTPHandlerOptions) http.Handler {
	var opt HTTPHandlerOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", r.requireAuth(opt.BearerToken, r.handleChatCompletionsWithOptions(opt)))
	mux.HandleFunc("/v1/models", r.requireAuth(opt.BearerToken, r.handleListModels))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return mux
}

// requireAuth wraps a handler with Bearer token authentication.
// When token is empty, the handler is returned as-is (no auth required).
func (r *Router) requireAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return next
	}
	expected := "Bearer " + token
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != expected {
			writeHTTPError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing Bearer token")
			return
		}
		next(w, req)
	}
}

func (r *Router) handleChatCompletionsWithOptions(opt HTTPHandlerOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.handleChatCompletions(w, req, opt)
	}
}

func (r *Router) handleChatCompletions(w http.ResponseWriter, req *http.Request, opt HTTPHandlerOptions) {
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
	if chatReq.MaxTokens != nil {
		writeHTTPError(w, http.StatusBadRequest, "unsupported", "max_tokens is not yet supported via HTTP; use the Go API directly")
		return
	}
	if chatReq.Temperature != nil {
		writeHTTPError(w, http.StatusBadRequest, "unsupported", "temperature is not yet supported via HTTP; use the Go API directly")
		return
	}

	if len(chatReq.Messages) == 0 {
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", "messages array is empty")
		return
	}

	activeRouter, modeErr := r.routerForHTTPMode(chatReq.Mode)
	if modeErr != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", modeErr.Error())
		return
	}
	defer activeRouter.persistHTTPStats(opt.StatsDir)

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
	var (
		content string
		usage   Usage
		result  RouteResult
		err     error
	)
	if requestedModel := strings.ToLower(strings.TrimSpace(chatReq.Model)); requestedModel != "" {
		if !containsString(activeRouter.registeredProviderNames(), requestedModel) {
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("unknown model %q; use GET /v1/models to discover supported provider names", chatReq.Model))
			return
		}
		content, usage, result, err = activeRouter.ChatWith(ctx, requestedModel, taskToBuildPhase(task), messages, system)
	} else {
		content, usage, result, err = activeRouter.Chat(ctx, task, messages, system)
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		if strings.Contains(err.Error(), "not configured") {
			status = http.StatusBadRequest
		}
		writeHTTPError(w, status, "provider_error", err.Error())
		return
	}
	responseModel := result.ModelID
	if strings.TrimSpace(responseModel) == "" {
		responseModel = result.Actual
	}

	resp := httpChatResponse{
		ID:      fmt.Sprintf("mw-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   responseModel,
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

func (r *Router) routerForHTTPMode(requestedMode string) (*Router, error) {
	modeName := strings.ToLower(strings.TrimSpace(requestedMode))
	if modeName == "" {
		return r, nil
	}

	mode, ok := ParseUsageMode(modeName)
	if !ok {
		return nil, fmt.Errorf("unknown mode %q; supported values are fast, balanced, power", requestedMode)
	}
	return r.cloneWithMode(mode), nil
}

func (r *Router) persistHTTPStats(statsDir string) {
	statsDir = strings.TrimSpace(statsDir)
	if statsDir == "" {
		return
	}
	if err := os.MkdirAll(statsDir, 0700); err != nil {
		return
	}
	_ = r.SaveStats(statsDir)
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
