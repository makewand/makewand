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

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
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

	// Authorizer enables scoped multi-token auth. When set, it takes precedence
	// over BearerToken and can restrict scopes, session prefixes, providers, and
	// modes on a per-token basis.
	Authorizer *serverauth.Authorizer

	// StatsDir enables routing stats persistence after chat requests.
	// When empty, HTTP requests do not read or write routing_stats.json.
	StatsDir string

	// AuditLogger receives one event per handled request when non-nil.
	AuditLogger serveraudit.Logger
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

func authorizerForHTTPOptions(opt HTTPHandlerOptions) *serverauth.Authorizer {
	if opt.Authorizer != nil {
		return opt.Authorizer
	}
	if strings.TrimSpace(opt.BearerToken) != "" {
		return serverauth.NewSingleTokenAuthorizer(opt.BearerToken)
	}
	return nil
}

func (r *Router) requireScope(authz *serverauth.Authorizer, scope string, logger serveraudit.Logger, next scopedHTTPHandler) http.HandlerFunc {
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
	defer func() {
		if auditEvent.DurationMS == 0 {
			auditEvent.DurationMS = time.Since(start).Milliseconds()
		}
		logHTTPAudit(opt.AuditLogger, auditEvent, auditEvent.Status, auditEvent.ActualProvider, auditEvent.Error)
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

	if chatReq.Stream {
		auditEvent.Status = http.StatusBadRequest
		auditEvent.Error = "streaming is not yet supported via HTTP; use the Go API directly"
		writeHTTPError(w, http.StatusBadRequest, "unsupported", "streaming is not yet supported via HTTP; use the Go API directly")
		return
	}
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
	auditEvent.Status = http.StatusOK
	auditEvent.ActualProvider = strings.TrimSpace(usage.Provider)
	if auditEvent.ActualProvider == "" {
		auditEvent.ActualProvider = strings.TrimSpace(result.Actual)
	}
	auditEvent.DurationMS = time.Since(start).Milliseconds()
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
