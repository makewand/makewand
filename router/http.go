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
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverhttp"
	"github.com/makewand/makewand/serverusage"
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

type httpStreamResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []httpStreamChoice `json:"choices"`
}

type httpStreamChoice struct {
	Index        int             `json:"index"`
	Delta        httpStreamDelta `json:"delta"`
	FinishReason string          `json:"finish_reason,omitempty"`
}

type httpStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
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

	// Authorizer enables scoped multi-token auth. When set, it takes precedence
	// over BearerToken and can restrict scopes, session prefixes, providers, and
	// modes on a per-token basis.
	Authorizer serverauth.RequestAuthorizer

	// StatsDir enables routing stats persistence after chat requests.
	// When empty, HTTP requests do not read or write routing_stats.json.
	StatsDir string

	// AuditLogger receives one event per handled request when non-nil.
	AuditLogger serveraudit.Logger

	// UsageLogger receives one structured usage entry per chat completion
	// request, including status, realized cost, and provider metadata.
	UsageLogger serverusage.Logger
}

// HTTPHandler returns an http.Handler that serves an OpenAI-compatible subset
// /v1/chat/completions endpoint backed by this Router.
//
// Supported features:
//   - POST /v1/chat/completions (streaming and non-streaming)
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
	authz := authorizerForHTTPOptions(opt)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", r.requireScope(authz, serverauth.ScopeChatInvoke, opt.AuditLogger, r.handleChatCompletionsWithOptions(opt)))
	mux.HandleFunc("/v1/models", r.requireScope(authz, serverauth.ScopeModelsRead, opt.AuditLogger, r.handleListModelsWithOptions(opt)))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return mux
}

type scopedHTTPHandler func(w http.ResponseWriter, req *http.Request, grant *serverauth.Grant)

func authorizerForHTTPOptions(opt HTTPHandlerOptions) serverauth.RequestAuthorizer {
	if opt.Authorizer != nil {
		return opt.Authorizer
	}
	if strings.TrimSpace(opt.BearerToken) != "" {
		return serverauth.NewSingleTokenAuthorizer(opt.BearerToken)
	}
	return nil
}

func (r *Router) requireScope(authz serverauth.RequestAuthorizer, scope string, logger serveraudit.Logger, next scopedHTTPHandler) http.HandlerFunc {
	if authz == nil {
		return func(w http.ResponseWriter, req *http.Request) {
			next(w, req, nil)
		}
	}
	return func(w http.ResponseWriter, req *http.Request) {
		grant, ok := authz.AuthenticateRequest(req)
		if !ok {
			writeHTTPError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing Bearer token")
			logHTTPAudit(logger, auditBaseEvent(req, nil, scope), http.StatusUnauthorized, "", "")
			return
		}
		if err := grant.CheckAndConsumeRequestAt(time.Now()); err != nil {
			writeHTTPError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
			logHTTPAudit(logger, auditBaseEvent(req, grant, scope), http.StatusTooManyRequests, "", err.Error())
			return
		}
		if !grant.AllowsScope(scope) {
			writeHTTPError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token does not allow scope %q", scope))
			logHTTPAudit(logger, auditBaseEvent(req, grant, scope), http.StatusForbidden, "", "")
			return
		}
		next(w, req, grant)
	}
}

func (r *Router) handleChatCompletionsWithOptions(opt HTTPHandlerOptions) scopedHTTPHandler {
	return func(w http.ResponseWriter, req *http.Request, grant *serverauth.Grant) {
		r.handleChatCompletions(w, req, opt, grant)
	}
}

func (r *Router) handleChatCompletions(w http.ResponseWriter, req *http.Request, opt HTTPHandlerOptions, grant *serverauth.Grant) {
	start := time.Now()
	auditEvent := auditBaseEvent(req, grant, serverauth.ScopeChatInvoke)
	auditEvent.Kind = "chat"
	usageEntry := usageBaseEntry(req, grant)
	defer func() {
		if auditEvent.DurationMS == 0 {
			auditEvent.DurationMS = time.Since(start).Milliseconds()
		}
		if usageEntry.DurationMS == 0 {
			usageEntry.DurationMS = auditEvent.DurationMS
		}
		if usageEntry.Status == 0 {
			usageEntry.Status = auditEvent.Status
		}
		if usageEntry.RequestedMode == "" {
			usageEntry.RequestedMode = auditEvent.RequestedMode
		}
		if usageEntry.RequestedModel == "" {
			usageEntry.RequestedModel = auditEvent.RequestedModel
		}
		if usageEntry.ActualProvider == "" {
			usageEntry.ActualProvider = auditEvent.ActualProvider
		}
		logHTTPAudit(opt.AuditLogger, auditEvent, auditEvent.Status, auditEvent.ActualProvider, auditEvent.Error)
		logHTTPUsage(opt.UsageLogger, usageEntry)
	}()

	if req.Method != http.MethodPost {
		auditEvent.Status = http.StatusMethodNotAllowed
		auditEvent.Error = "only POST is supported"
		writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}

	var chatReq httpChatRequest
	if err := json.NewDecoder(req.Body).Decode(&chatReq); err != nil {
		auditEvent.Status = http.StatusBadRequest
		auditEvent.Error = "invalid JSON: " + err.Error()
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	auditEvent.RequestedMode = strings.ToLower(strings.TrimSpace(chatReq.Mode))
	auditEvent.RequestedModel = strings.ToLower(strings.TrimSpace(chatReq.Model))
	usageEntry.RequestedMode = auditEvent.RequestedMode
	usageEntry.RequestedModel = auditEvent.RequestedModel
	usageEntry.Stream = chatReq.Stream

	// max_tokens and temperature are silently ignored — they are standard
	// OpenAI fields that clients commonly include. Rejecting them would
	// break otherwise valid requests.

	if len(chatReq.Messages) == 0 {
		auditEvent.Status = http.StatusBadRequest
		auditEvent.Error = "messages array is empty"
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", "messages array is empty")
		return
	}

	activeRouter, modeErr := r.routerForHTTPMode(chatReq.Mode)
	if modeErr != nil {
		auditEvent.Status = http.StatusBadRequest
		auditEvent.Error = modeErr.Error()
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", modeErr.Error())
		return
	}

	effectiveMode := activeRouter.effectiveMode().String()
	auditEvent.RequestedMode = effectiveMode
	usageEntry.RequestedMode = effectiveMode
	if !grant.AllowsMode(effectiveMode) {
		auditEvent.Status = http.StatusForbidden
		auditEvent.Error = fmt.Sprintf("token is not permitted to use mode %q", effectiveMode)
		writeHTTPError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token is not permitted to use mode %q", effectiveMode))
		return
	}

	requestedModel := strings.ToLower(strings.TrimSpace(chatReq.Model))
	if requestedModel != "" && !grant.AllowsProvider(requestedModel) {
		auditEvent.Status = http.StatusForbidden
		auditEvent.Error = fmt.Sprintf("token is not permitted to use provider %q", chatReq.Model)
		writeHTTPError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token is not permitted to use provider %q", chatReq.Model))
		return
	}
	if allowedProviders := grant.AllowedProviders(); len(allowedProviders) > 0 {
		activeRouter = activeRouter.cloneWithProviderAllowlist(allowedProviders)
		if len(activeRouter.registeredProviderNames()) == 0 {
			auditEvent.Status = http.StatusForbidden
			auditEvent.Error = "token is not permitted to use any configured providers"
			writeHTTPError(w, http.StatusForbidden, "forbidden", "token is not permitted to use any configured providers")
			return
		}
	}
	if err := grant.CheckCostBudgetAt(time.Now()); err != nil {
		auditEvent.Status = http.StatusTooManyRequests
		auditEvent.Error = err.Error()
		writeHTTPError(w, http.StatusTooManyRequests, "budget_exceeded", err.Error())
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

	if chatReq.Stream {
		r.handleChatCompletionsStream(w, req, activeRouter, grant, requestedModel, task, messages, system, &auditEvent, &usageEntry)
		return
	}

	ctx := req.Context()
	var (
		content string
		usage   Usage
		result  RouteResult
		err     error
	)
	if requestedModel != "" {
		if !containsString(activeRouter.registeredProviderNames(), requestedModel) {
			auditEvent.Status = http.StatusBadRequest
			auditEvent.Error = fmt.Sprintf("unknown model %q; use GET /v1/models to discover supported provider names", chatReq.Model)
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
		auditEvent.Status = status
		auditEvent.Error = err.Error()
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
	grant.RecordCostAt(time.Now(), usage.Cost)
	auditEvent.Status = http.StatusOK
	auditEvent.ActualProvider = strings.TrimSpace(usage.Provider)
	if auditEvent.ActualProvider == "" {
		auditEvent.ActualProvider = strings.TrimSpace(result.Actual)
	}
	usageEntry.Status = http.StatusOK
	usageEntry.ActualProvider = auditEvent.ActualProvider
	usageEntry.PromptTokens = usage.InputTokens
	usageEntry.CompletionTokens = usage.OutputTokens
	usageEntry.CostUSD = usage.Cost
	auditEvent.PromptTokens = usage.InputTokens
	auditEvent.CompletionTokens = usage.OutputTokens
	auditEvent.CostUSD = usage.Cost
	auditEvent.DurationMS = time.Since(start).Milliseconds()
}

func (r *Router) handleChatCompletionsStream(w http.ResponseWriter, req *http.Request, activeRouter *Router, grant *serverauth.Grant, requestedModel string, task TaskType, messages []Message, system string, auditEvent *serveraudit.Event, usageEntry *serverusage.Entry) {
	if auditEvent == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		auditEvent.Status = http.StatusInternalServerError
		auditEvent.Error = "streaming is unavailable because the response writer does not support flushing"
		writeHTTPError(w, http.StatusInternalServerError, "internal_error", auditEvent.Error)
		return
	}

	ctx := req.Context()
	var (
		stream <-chan StreamChunk
		result RouteResult
		err    error
	)
	if requestedModel != "" {
		if !containsString(activeRouter.registeredProviderNames(), requestedModel) {
			auditEvent.Status = http.StatusBadRequest
			auditEvent.Error = fmt.Sprintf("unknown model %q; use GET /v1/models to discover supported provider names", requestedModel)
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", auditEvent.Error)
			return
		}
		stream, result, err = activeRouter.ChatStreamWith(ctx, requestedModel, taskToBuildPhase(task), messages, system)
	} else {
		stream, result, err = activeRouter.ChatStream(ctx, task, messages, system)
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		if strings.Contains(err.Error(), "not configured") {
			status = http.StatusBadRequest
		}
		auditEvent.Status = status
		auditEvent.Error = err.Error()
		writeHTTPError(w, status, "provider_error", err.Error())
		return
	}

	responseModel := result.ModelID
	if strings.TrimSpace(responseModel) == "" {
		responseModel = result.Actual
	}
	auditEvent.ActualProvider = strings.TrimSpace(result.Actual)
	if usageEntry != nil {
		usageEntry.ActualProvider = auditEvent.ActualProvider
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	streamID := fmt.Sprintf("mw-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	sentRole := false
	for chunk := range stream {
		if chunk.Error != nil {
			auditEvent.Status = http.StatusOK
			auditEvent.Error = chunk.Error.Error()
			_ = writeSSEData(w, map[string]any{
				"error": map[string]any{
					"message": chunk.Error.Error(),
					"type":    "error",
					"code":    "stream_error",
				},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		if chunk.Content != "" || (!sentRole && !chunk.Done) {
			delta := httpStreamDelta{Content: chunk.Content}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			if err := writeSSEData(w, httpStreamResponse{
				ID:      streamID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   responseModel,
				Choices: []httpStreamChoice{{
					Index: 0,
					Delta: delta,
				}},
			}); err != nil {
				auditEvent.Status = http.StatusOK
				auditEvent.Error = err.Error()
				return
			}
			flusher.Flush()
		}

		if chunk.Done {
			_ = writeSSEData(w, httpStreamResponse{
				ID:      streamID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   responseModel,
				Choices: []httpStreamChoice{{
					Index:        0,
					Delta:        httpStreamDelta{},
					FinishReason: "stop",
				}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			auditEvent.Status = http.StatusOK
			if usageEntry != nil {
				usageEntry.Status = http.StatusOK
			}
			return
		}
	}

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
	auditEvent.Status = http.StatusOK
	if usageEntry != nil {
		usageEntry.Status = http.StatusOK
	}
}

func writeSSEData(w http.ResponseWriter, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
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
	r.handleListModelsWithGrant(w, req, nil, nil)
}

func (r *Router) handleListModelsWithOptions(opt HTTPHandlerOptions) scopedHTTPHandler {
	return func(w http.ResponseWriter, req *http.Request, grant *serverauth.Grant) {
		r.handleListModelsWithGrant(w, req, grant, opt.AuditLogger)
	}
}

func (r *Router) handleListModelsWithGrant(w http.ResponseWriter, req *http.Request, grant *serverauth.Grant, logger serveraudit.Logger) {
	start := time.Now()
	auditEvent := auditBaseEvent(req, grant, serverauth.ScopeModelsRead)
	auditEvent.Kind = "models"
	defer func() {
		if auditEvent.DurationMS == 0 {
			auditEvent.DurationMS = time.Since(start).Milliseconds()
		}
		logHTTPAudit(logger, auditEvent, auditEvent.Status, "", auditEvent.Error)
	}()

	if req.Method != http.MethodGet {
		auditEvent.Status = http.StatusMethodNotAllowed
		auditEvent.Error = "only GET is supported"
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

	activeRouter := r
	if allowedProviders := grant.AllowedProviders(); len(allowedProviders) > 0 {
		activeRouter = r.cloneWithProviderAllowlist(allowedProviders)
	}

	avail := activeRouter.Available()
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
	auditEvent.Status = http.StatusOK
	auditEvent.DurationMS = time.Since(start).Milliseconds()
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

func auditBaseEvent(req *http.Request, grant *serverauth.Grant, scope string) serveraudit.Event {
	event := serveraudit.Event{
		Timestamp: time.Now().UTC(),
		RequestID: serverhttp.RequestIDFromRequest(req),
		Scope:     scope,
	}
	if req != nil {
		event.Kind = httpAuditKind(req.URL.Path)
		event.Method = req.Method
		event.Path = req.URL.Path
	}
	if grant != nil {
		event.TokenID = grant.TokenID()
		event.TokenDescription = grant.Description()
	}
	return event
}

func usageBaseEntry(req *http.Request, grant *serverauth.Grant) serverusage.Entry {
	entry := serverusage.Entry{
		Timestamp: time.Now().UTC(),
		RequestID: serverhttp.RequestIDFromRequest(req),
	}
	if grant != nil {
		entry.TokenID = grant.TokenID()
		entry.TokenDescription = grant.Description()
	}
	return entry
}

func logHTTPAudit(logger serveraudit.Logger, event serveraudit.Event, status int, actualProvider, errText string) {
	if logger == nil {
		return
	}
	if event.Status == 0 {
		event.Status = status
	}
	if event.ActualProvider == "" {
		event.ActualProvider = actualProvider
	}
	if event.Error == "" {
		event.Error = errText
	}
	logger.Log(event)
}

func logHTTPUsage(logger serverusage.Logger, entry serverusage.Entry) {
	if logger == nil {
		return
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	logger.Log(entry)
}

func httpAuditKind(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "chat"
	case "/v1/models":
		return "models"
	default:
		return "http"
	}
}
