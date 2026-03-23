package serveradmin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverhttp"
	"github.com/makewand/makewand/serverusage"
)

type HandlerOptions struct {
	Authorizer   serverauth.RequestAuthorizer
	TokenManager serverauth.TokenManager
	AuditPath    string
	AuditLogger  serveraudit.Logger
	UsagePath    string
	UsageStore   serverusage.Reader
	UserStore    router.UserManager
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type issueTokenRequest struct {
	ID                 string    `json:"id,omitempty"`
	Token              string    `json:"token,omitempty"`
	Description        string    `json:"description,omitempty"`
	Scopes             []string  `json:"scopes,omitempty"`
	WorkspacePrefixes  []string  `json:"workspace_prefixes,omitempty"`
	AllowedProviders   []string  `json:"allowed_providers,omitempty"`
	AllowedModes       []string  `json:"allowed_modes,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
	MaxRequestsPerHour int       `json:"max_requests_per_hour,omitempty"`
	MaxRequestsPerDay  int       `json:"max_requests_per_day,omitempty"`
	MaxCostUSDPerDay   float64   `json:"max_cost_usd_per_day,omitempty"`
	MaxCostUSDPerMonth float64   `json:"max_cost_usd_per_month,omitempty"`
}

type updateUserRoleRequest struct {
	Role string `json:"role"`
}

func NewHandler(opts HandlerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/v1/admin/tokens":
			handleTokens(w, req, opts)
		case revokeTarget(req.URL.Path) != "":
			handleRevokeToken(w, req, opts)
		case req.URL.Path == "/v1/admin/audit/summary":
			handleAuditSummary(w, req, opts)
		case req.URL.Path == "/v1/admin/audit/events":
			handleAuditEvents(w, req, opts)
		case req.URL.Path == "/v1/admin/usage/summary":
			handleUsageSummary(w, req, opts)
		case req.URL.Path == "/v1/admin/usage/events":
			handleUsageEvents(w, req, opts)
		case req.URL.Path == "/v1/admin/users":
			handleUsers(w, req, opts)
		case userActionTarget(req.URL.Path) != "":
			handleUserAction(w, req, opts)
		default:
			http.NotFound(w, req)
		}
	})
}

func handleTokens(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method == http.MethodGet {
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminTokensRead, "admin_tokens")
		if !ok {
			return
		}
		_ = grant
		writeJSON(w, http.StatusOK, map[string]any{
			"data": opts.TokenManager.TokenRules(),
		})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensRead, "admin_tokens", http.StatusOK, "", 0, 0, 0)
		return
	}
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminTokensWrite, "admin_tokens")
	if !ok {
		return
	}
	var payload issueTokenRequest
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", http.StatusBadRequest, "invalid JSON: "+err.Error(), 0, 0, 0)
		return
	}
	rule := serverauth.TokenRule{
		ID:                 strings.TrimSpace(payload.ID),
		Token:              strings.TrimSpace(payload.Token),
		Description:        strings.TrimSpace(payload.Description),
		Scopes:             append([]string(nil), payload.Scopes...),
		WorkspacePrefixes:  append([]string(nil), payload.WorkspacePrefixes...),
		AllowedProviders:   append([]string(nil), payload.AllowedProviders...),
		AllowedModes:       append([]string(nil), payload.AllowedModes...),
		ExpiresAt:          payload.ExpiresAt,
		MaxRequestsPerHour: payload.MaxRequestsPerHour,
		MaxRequestsPerDay:  payload.MaxRequestsPerDay,
		MaxCostUSDPerDay:   payload.MaxCostUSDPerDay,
		MaxCostUSDPerMonth: payload.MaxCostUSDPerMonth,
	}
	if len(rule.Scopes) == 0 {
		rule.Scopes = serverauth.AllClientScopes()
	}
	view, tokenValue, err := opts.TokenManager.Issue(rule)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token_id": view.ID,
		"token":    tokenValue,
		"rule":     view,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", http.StatusCreated, "", 0, 0, 0)
}

func handleRevokeToken(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminTokensWrite, "admin_tokens")
	if !ok {
		return
	}
	tokenID := revokeTarget(req.URL.Path)
	if tokenID == "" {
		http.NotFound(w, req)
		return
	}
	if err := opts.TokenManager.Revoke(tokenID); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", status, err.Error(), 0, 0, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token_id": tokenID,
		"revoked":  true,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", http.StatusOK, "", 0, 0, 0)
}

func handleAuditSummary(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminAuditRead, "admin_audit")
	if !ok {
		return
	}
	filter, err := filterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	events, err := loadAuditEvents(opts.AuditPath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	summary := serveraudit.SummarizeEvents(events)
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    opts.AuditPath,
		"summary": summary,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusOK, "", 0, 0, 0)
}

func handleAuditEvents(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminAuditRead, "admin_audit")
	if !ok {
		return
	}
	filter, err := filterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	events, err := loadAuditEvents(opts.AuditPath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": opts.AuditPath,
		"data": events,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusOK, "", 0, 0, 0)
}

func handleUsageSummary(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsageRead, "admin_usage")
	if !ok {
		return
	}
	filter, err := usageFilterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	entries, err := loadUsageEntries(opts.UsageStore, opts.UsagePath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	summary := serverusage.SummarizeEntries(entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"path":  opts.UsagePath,
		"usage": summary,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusOK, "", 0, 0, 0)
}

func handleUsageEvents(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsageRead, "admin_usage")
	if !ok {
		return
	}
	filter, err := usageFilterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	entries, err := loadUsageEntries(opts.UsageStore, opts.UsagePath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": opts.UsagePath,
		"data": entries,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusOK, "", 0, 0, 0)
}

func handleUsers(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersRead, "admin_users")
	if !ok {
		return
	}
	if opts.UserStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "user management is unavailable")
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_users", http.StatusNotFound, "user management is unavailable", 0, 0, 0)
		return
	}
	users, err := opts.UserStore.ListUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_users", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": users})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_users", http.StatusOK, "", 0, 0, 0)
}

func handleUserAction(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersWrite, "admin_users")
	if !ok {
		return
	}
	if opts.UserStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "user management is unavailable")
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_users", http.StatusNotFound, "user management is unavailable", 0, 0, 0)
		return
	}
	userID, action := splitUserActionTarget(req.URL.Path)
	if userID == "" || action == "" {
		http.NotFound(w, req)
		return
	}

	var (
		user *router.User
		err  error
	)
	switch action {
	case "activate":
		user, err = opts.UserStore.SetUserActive(userID, true)
	case "deactivate":
		user, err = opts.UserStore.SetUserActive(userID, false)
	case "role":
		var payload updateUserRoleRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if decodeErr := dec.Decode(&payload); decodeErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+decodeErr.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_users", http.StatusBadRequest, "invalid JSON: "+decodeErr.Error(), 0, 0, 0)
			return
		}
		user, err = opts.UserStore.SetUserRole(userID, payload.Role)
	default:
		http.NotFound(w, req)
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, router.ErrUserNotFound):
			status = http.StatusNotFound
		case errors.Is(err, router.ErrInvalidUserRole):
			status = http.StatusBadRequest
		default:
			status = http.StatusInternalServerError
		}
		writeError(w, status, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_users", status, err.Error(), 0, 0, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": userID,
		"action":  action,
		"user":    user.View(),
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_users", http.StatusOK, "", 0, 0, 0)
}

func authenticateAdmin(w http.ResponseWriter, req *http.Request, opts HandlerOptions, scope, kind string) (*serverauth.Grant, bool) {
	if opts.TokenManager == nil || opts.Authorizer == nil {
		writeError(w, http.StatusNotFound, "not_found", "admin API is unavailable without --auth-config")
		logAdminEvent(opts.AuditLogger, req, nil, scope, kind, http.StatusNotFound, "admin API is unavailable without --auth-config", 0, 0, 0)
		return nil, false
	}
	grant, ok := opts.Authorizer.AuthenticateRequest(req)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing Bearer token")
		logAdminEvent(opts.AuditLogger, req, nil, scope, kind, http.StatusUnauthorized, "invalid or missing Bearer token", 0, 0, 0)
		return nil, false
	}
	if err := grant.CheckAndConsumeRequestAt(time.Now()); err != nil {
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, scope, kind, http.StatusTooManyRequests, err.Error(), 0, 0, 0)
		return nil, false
	}
	if !grant.AllowsScope(scope) {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token does not allow scope %q", scope))
		logAdminEvent(opts.AuditLogger, req, grant, scope, kind, http.StatusForbidden, fmt.Sprintf("token does not allow scope %q", scope), 0, 0, 0)
		return nil, false
	}
	return grant, true
}

func loadAuditEvents(path string, filter serveraudit.Filter) ([]serveraudit.Event, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	events, err := serveraudit.LoadEvents(path, filter)
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	return events, err
}

func loadUsageEntries(store serverusage.Reader, path string, filter serverusage.Filter) ([]serverusage.Entry, error) {
	if store != nil {
		return store.Load(filter)
	}
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	entries, err := serverusage.LoadEntries(path, filter)
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	return entries, err
}

func filterFromQuery(req *http.Request) (serveraudit.Filter, error) {
	query := req.URL.Query()
	status, err := parseOptionalInt(query.Get("status"))
	if err != nil {
		return serveraudit.Filter{}, fmt.Errorf("parse status: %w", err)
	}
	limit, err := parseOptionalInt(query.Get("limit"))
	if err != nil {
		return serveraudit.Filter{}, fmt.Errorf("parse limit: %w", err)
	}
	now := time.Now().UTC()
	since, err := parseTimeValue(query.Get("since"), now)
	if err != nil {
		return serveraudit.Filter{}, fmt.Errorf("parse since: %w", err)
	}
	until, err := parseTimeValue(query.Get("until"), now)
	if err != nil {
		return serveraudit.Filter{}, fmt.Errorf("parse until: %w", err)
	}
	return serveraudit.Filter{
		TokenID:     strings.TrimSpace(query.Get("token_id")),
		Kind:        strings.TrimSpace(query.Get("kind")),
		WorkspaceID: strings.TrimSpace(query.Get("workspace")),
		Status:      status,
		Limit:       limit,
		Since:       since,
		Until:       until,
	}, nil
}

func usageFilterFromQuery(req *http.Request) (serverusage.Filter, error) {
	query := req.URL.Query()
	status, err := parseOptionalInt(query.Get("status"))
	if err != nil {
		return serverusage.Filter{}, fmt.Errorf("parse status: %w", err)
	}
	limit, err := parseOptionalInt(query.Get("limit"))
	if err != nil {
		return serverusage.Filter{}, fmt.Errorf("parse limit: %w", err)
	}
	now := time.Now().UTC()
	since, err := parseTimeValue(query.Get("since"), now)
	if err != nil {
		return serverusage.Filter{}, fmt.Errorf("parse since: %w", err)
	}
	until, err := parseTimeValue(query.Get("until"), now)
	if err != nil {
		return serverusage.Filter{}, fmt.Errorf("parse until: %w", err)
	}
	return serverusage.Filter{
		RequestID:  strings.TrimSpace(query.Get("request_id")),
		TokenID:    strings.TrimSpace(query.Get("token_id")),
		Provider:   strings.TrimSpace(query.Get("provider")),
		Status:     status,
		Limit:      limit,
		Since:      since,
		Until:      until,
		StreamOnly: strings.EqualFold(strings.TrimSpace(query.Get("stream_only")), "true"),
	}, nil
}

func parseOptionalInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.Atoi(raw)
}

func parseTimeValue(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if duration, err := time.ParseDuration(raw); err == nil {
		return now.Add(-duration), nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return value.UTC(), nil
}

func revokeTarget(path string) string {
	const prefix = "/v1/admin/tokens/"
	const suffix = "/revoke"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	value = strings.Trim(value, "/")
	return strings.TrimSpace(value)
}

func userActionTarget(path string) string {
	const prefix = "/v1/admin/users/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	id, action := splitUserActionTarget(path)
	if id == "" || action == "" {
		return ""
	}
	return path
}

func splitUserActionTarget(path string) (string, string) {
	const prefix = "/v1/admin/users/"
	if !strings.HasPrefix(path, prefix) {
		return "", ""
	}
	value := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return "", ""
	}
	action := strings.TrimSpace(parts[1])
	switch action {
	case "activate", "deactivate", "role":
		return strings.TrimSpace(parts[0]), action
	default:
		return "", ""
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	resp := errorResponse{}
	resp.Error.Message = message
	resp.Error.Type = "error"
	resp.Error.Code = code
	writeJSON(w, status, resp)
}

func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func logAdminEvent(logger serveraudit.Logger, req *http.Request, grant *serverauth.Grant, scope, kind string, status int, errText string, promptTokens, completionTokens int, costUSD float64) {
	if logger == nil {
		return
	}
	event := serveraudit.Event{
		Timestamp:        time.Now().UTC(),
		RequestID:        serverhttp.RequestIDFromRequest(req),
		Kind:             kind,
		Scope:            scope,
		Status:           status,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CostUSD:          costUSD,
		Error:            errText,
	}
	if req != nil {
		event.Method = req.Method
		event.Path = req.URL.Path
	}
	if grant != nil {
		event.TokenID = grant.TokenID()
		event.TokenDescription = grant.Description()
	}
	logger.Log(event)
}
