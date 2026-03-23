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
	"errors"
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

type httpResponsesRequest struct {
	Model           string            `json:"model,omitempty"`
	Mode            string            `json:"mode,omitempty"`
	Input           any               `json:"input"`
	Instructions    string            `json:"instructions,omitempty"`
	MaxOutputTokens *int              `json:"max_output_tokens,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
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

type httpResponsesResponse struct {
	ID         string               `json:"id"`
	Object     string               `json:"object"`
	CreatedAt  int64                `json:"created_at"`
	Status     string               `json:"status"`
	Model      string               `json:"model"`
	Output     []httpResponseOutput `json:"output"`
	OutputText string               `json:"output_text"`
	Usage      httpUsageResponse    `json:"usage"`
	Metadata   map[string]string    `json:"metadata,omitempty"`
}

type httpResponseOutput struct {
	ID      string                `json:"id,omitempty"`
	Type    string                `json:"type"`
	Role    string                `json:"role,omitempty"`
	Content []httpResponseContent `json:"content,omitempty"`
}

type httpResponseContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
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

	// UserTokenManager issues login tokens for /v1/users/login when the users
	// extension is enabled.
	UserTokenManager serverauth.TokenManager
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
	mux.HandleFunc("/v1/responses", r.requireScope(authz, serverauth.ScopeChatInvoke, opt.AuditLogger, r.handleResponsesWithOptions(opt)))
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

func (r *Router) handleResponsesWithOptions(opt HTTPHandlerOptions) scopedHTTPHandler {
	return func(w http.ResponseWriter, req *http.Request, grant *serverauth.Grant) {
		r.handleResponses(w, req, opt, grant)
	}
}

func (r *Router) handleResponses(w http.ResponseWriter, req *http.Request, opt HTTPHandlerOptions, grant *serverauth.Grant) {
	start := time.Now()
	auditEvent := auditBaseEvent(req, grant, serverauth.ScopeChatInvoke)
	auditEvent.Kind = "responses"
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

	var responsesReq httpResponsesRequest
	if err := json.NewDecoder(req.Body).Decode(&responsesReq); err != nil {
		auditEvent.Status = http.StatusBadRequest
		auditEvent.Error = "invalid JSON: " + err.Error()
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	usageEntry.RequestedMode = strings.ToLower(strings.TrimSpace(responsesReq.Mode))
	usageEntry.RequestedModel = strings.ToLower(strings.TrimSpace(responsesReq.Model))
	auditEvent.RequestedMode = usageEntry.RequestedMode
	auditEvent.RequestedModel = usageEntry.RequestedModel
	if responsesReq.Stream {
		auditEvent.Status = http.StatusBadRequest
		auditEvent.Error = "stream is not yet supported on /v1/responses"
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", "stream is not yet supported on /v1/responses")
		return
	}
	chatReq, err := responsesRequestToChatRequest(responsesReq)
	if err != nil {
		auditEvent.Status = http.StatusBadRequest
		auditEvent.Error = err.Error()
		writeHTTPError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	prepared, err := r.prepareHTTPChat(chatReq, grant)
	if err != nil {
		var httpErr *httpStatusError
		if !errors.As(err, &httpErr) {
			httpErr = &httpStatusError{Status: http.StatusInternalServerError, Code: "internal_error", Message: err.Error()}
		}
		auditEvent.Status = httpErr.Status
		auditEvent.Error = httpErr.Message
		writeHTTPError(w, httpErr.Status, httpErr.Code, httpErr.Message)
		return
	}
	auditEvent.RequestedMode = prepared.EffectiveMode
	usageEntry.RequestedMode = prepared.EffectiveMode
	defer prepared.Router.persistHTTPStats(opt.StatsDir)

	ctx := req.Context()
	var (
		content string
		usage   Usage
		result  RouteResult
	)
	if prepared.RequestedModel != "" {
		if !containsString(prepared.Router.registeredProviderNames(), prepared.RequestedModel) {
			auditEvent.Status = http.StatusBadRequest
			auditEvent.Error = fmt.Sprintf("unknown model %q; use GET /v1/models to discover supported provider names", responsesReq.Model)
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("unknown model %q; use GET /v1/models to discover supported provider names", responsesReq.Model))
			return
		}
		content, usage, result, err = prepared.Router.ChatWith(ctx, prepared.RequestedModel, taskToBuildPhase(prepared.Task), prepared.Messages, prepared.System)
	} else {
		content, usage, result, err = prepared.Router.Chat(ctx, prepared.Task, prepared.Messages, prepared.System)
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
	if grant != nil {
		grant.RecordCostAt(time.Now(), usage.Cost)
	}

	responseModel := result.ModelID
	if strings.TrimSpace(responseModel) == "" {
		responseModel = result.Actual
	}
	auditEvent.Status = http.StatusOK
	auditEvent.ActualProvider = strings.TrimSpace(usage.Provider)
	if auditEvent.ActualProvider == "" {
		auditEvent.ActualProvider = strings.TrimSpace(result.Actual)
	}
	auditEvent.PromptTokens = usage.InputTokens
	auditEvent.CompletionTokens = usage.OutputTokens
	auditEvent.CostUSD = usage.Cost
	usageEntry.Status = http.StatusOK
	usageEntry.ActualProvider = auditEvent.ActualProvider
	usageEntry.PromptTokens = usage.InputTokens
	usageEntry.CompletionTokens = usage.OutputTokens
	usageEntry.CostUSD = usage.Cost
	resp := httpResponsesResponse{
		ID:         fmt.Sprintf("resp_%d", time.Now().UnixNano()),
		Object:     "response",
		CreatedAt:  time.Now().Unix(),
		Status:     "completed",
		Model:      responseModel,
		Output:     buildResponsesOutput(content),
		OutputText: content,
		Usage: httpUsageResponse{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			TotalTokens:      usage.InputTokens + usage.OutputTokens,
		},
		Metadata: responsesReq.Metadata,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
	prepared, err := r.prepareHTTPChat(chatReq, grant)
	if err != nil {
		var httpErr *httpStatusError
		if !errors.As(err, &httpErr) {
			httpErr = &httpStatusError{Status: http.StatusInternalServerError, Code: "internal_error", Message: err.Error()}
		}
		auditEvent.Status = httpErr.Status
		auditEvent.Error = httpErr.Message
		writeHTTPError(w, httpErr.Status, httpErr.Code, httpErr.Message)
		return
	}
	auditEvent.RequestedMode = prepared.EffectiveMode
	usageEntry.RequestedMode = prepared.EffectiveMode
	defer prepared.Router.persistHTTPStats(opt.StatsDir)

	if chatReq.Stream {
		r.handleChatCompletionsStream(w, req, prepared.Router, grant, prepared.RequestedModel, prepared.Task, prepared.Messages, prepared.System, &auditEvent, &usageEntry)
		return
	}

	ctx := req.Context()
	var (
		content string
		usage   Usage
		result  RouteResult
	)
	if prepared.RequestedModel != "" {
		if !containsString(prepared.Router.registeredProviderNames(), prepared.RequestedModel) {
			auditEvent.Status = http.StatusBadRequest
			auditEvent.Error = fmt.Sprintf("unknown model %q; use GET /v1/models to discover supported provider names", chatReq.Model)
			writeHTTPError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("unknown model %q; use GET /v1/models to discover supported provider names", chatReq.Model))
			return
		}
		content, usage, result, err = prepared.Router.ChatWith(ctx, prepared.RequestedModel, taskToBuildPhase(prepared.Task), prepared.Messages, prepared.System)
	} else {
		content, usage, result, err = prepared.Router.Chat(ctx, prepared.Task, prepared.Messages, prepared.System)
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

type preparedHTTPChat struct {
	Router         *Router
	RequestedModel string
	Task           TaskType
	Messages       []Message
	System         string
	EffectiveMode  string
}

type httpStatusError struct {
	Status  int
	Code    string
	Message string
}

func (e *httpStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (r *Router) prepareHTTPChat(chatReq httpChatRequest, grant *serverauth.Grant) (*preparedHTTPChat, error) {
	if len(chatReq.Messages) == 0 {
		return nil, &httpStatusError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "messages array is empty"}
	}
	activeRouter, err := r.routerForHTTPMode(chatReq.Mode)
	if err != nil {
		return nil, &httpStatusError{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}
	effectiveMode := activeRouter.effectiveMode().String()
	if !grant.AllowsMode(effectiveMode) {
		return nil, &httpStatusError{
			Status:  http.StatusForbidden,
			Code:    "forbidden",
			Message: fmt.Sprintf("token is not permitted to use mode %q", effectiveMode),
		}
	}

	requestedModel := strings.ToLower(strings.TrimSpace(chatReq.Model))
	if requestedModel != "" && !grant.AllowsProvider(requestedModel) {
		return nil, &httpStatusError{
			Status:  http.StatusForbidden,
			Code:    "forbidden",
			Message: fmt.Sprintf("token is not permitted to use provider %q", chatReq.Model),
		}
	}
	if allowedProviders := grant.AllowedProviders(); len(allowedProviders) > 0 {
		activeRouter = activeRouter.cloneWithProviderAllowlist(allowedProviders)
		if len(activeRouter.registeredProviderNames()) == 0 {
			return nil, &httpStatusError{
				Status:  http.StatusForbidden,
				Code:    "forbidden",
				Message: "token is not permitted to use any configured providers",
			}
		}
	}
	if err := grant.CheckCostBudgetAt(time.Now()); err != nil {
		return nil, &httpStatusError{Status: http.StatusTooManyRequests, Code: "budget_exceeded", Message: err.Error()}
	}

	messages := make([]Message, 0, len(chatReq.Messages))
	var system string
	for _, msg := range chatReq.Messages {
		if msg.Role == "system" {
			system = msg.Content
			continue
		}
		messages = append(messages, Message{Role: msg.Role, Content: msg.Content})
	}

	task := TaskCode
	for i := len(chatReq.Messages) - 1; i >= 0; i-- {
		if chatReq.Messages[i].Role == "user" {
			task = ClassifyTask(chatReq.Messages[i].Content)
			break
		}
	}
	return &preparedHTTPChat{
		Router:         activeRouter,
		RequestedModel: requestedModel,
		Task:           task,
		Messages:       messages,
		System:         system,
		EffectiveMode:  effectiveMode,
	}, nil
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

func responsesRequestToChatRequest(req httpResponsesRequest) (httpChatRequest, error) {
	messages, err := responsesInputToMessages(req.Input, req.Instructions)
	if err != nil {
		return httpChatRequest{}, err
	}
	return httpChatRequest{
		Model:       req.Model,
		Mode:        req.Mode,
		Messages:    messages,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
	}, nil
}

func responsesInputToMessages(input any, instructions string) ([]httpMessage, error) {
	messages := make([]httpMessage, 0, 4)
	if strings.TrimSpace(instructions) != "" {
		messages = append(messages, httpMessage{Role: "system", Content: instructions})
	}
	appendUser := func(text string) {
		text = strings.TrimSpace(text)
		if text != "" {
			messages = append(messages, httpMessage{Role: "user", Content: text})
		}
	}

	switch value := input.(type) {
	case string:
		appendUser(value)
	case []any:
		for _, item := range value {
			role, content, err := responseInputItemToMessage(item)
			if err != nil {
				return nil, err
			}
			messages = append(messages, httpMessage{Role: role, Content: content})
		}
	case map[string]any:
		role, content, err := responseInputItemToMessage(value)
		if err != nil {
			return nil, err
		}
		messages = append(messages, httpMessage{Role: role, Content: content})
	default:
		return nil, fmt.Errorf("unsupported input payload for /v1/responses")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	return messages, nil
}

func responseInputItemToMessage(item any) (string, string, error) {
	switch value := item.(type) {
	case string:
		return "user", strings.TrimSpace(value), nil
	case map[string]any:
		role := strings.TrimSpace(stringValue(value["role"]))
		if role == "" {
			role = "user"
		}
		if content, ok := value["content"].(string); ok {
			return role, strings.TrimSpace(content), nil
		}
		if contentList, ok := value["content"].([]any); ok {
			parts := make([]string, 0, len(contentList))
			for _, entry := range contentList {
				switch content := entry.(type) {
				case string:
					parts = append(parts, content)
				case map[string]any:
					parts = append(parts, stringValue(content["text"]))
				}
			}
			return role, strings.TrimSpace(strings.Join(parts, "\n")), nil
		}
		if text := stringValue(value["text"]); strings.TrimSpace(text) != "" {
			return role, strings.TrimSpace(text), nil
		}
		return "", "", fmt.Errorf("input item content is empty")
	default:
		return "", "", fmt.Errorf("unsupported input item")
	}
}

func buildResponsesOutput(content string) []httpResponseOutput {
	return []httpResponseOutput{{
		ID:   fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		Type: "message",
		Role: "assistant",
		Content: []httpResponseContent{{
			Type: "output_text",
			Text: content,
		}},
	}}
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
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
	case "/v1/responses":
		return "responses"
	case "/v1/models":
		return "models"
	default:
		return "http"
	}
}
