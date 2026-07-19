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
	"github.com/makewand/makewand/serverteam"
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
	TeamStore    serverteam.Store
	SessionMgr   *SessionManager
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

const maxAdminJSONBodyBytes = 1 << 20 // 1 MiB

type issueTokenRequest struct {
	ID                 string    `json:"id,omitempty"`
	Token              string    `json:"token,omitempty"`
	Description        string    `json:"description,omitempty"`
	UserID             string    `json:"user_id,omitempty"`
	OrganizationID     string    `json:"organization_id,omitempty"`
	ProjectID          string    `json:"project_id,omitempty"`
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

type updateUserPasswordRequest struct {
	Password string `json:"password"`
}

func NewHandler(opts HandlerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/v1/admin/session/login":
			if opts.SessionMgr == nil {
				http.NotFound(w, req)
				return
			}
			opts.SessionMgr.HandleSessionLogin(w, req)
		case req.URL.Path == "/v1/admin/session/logout":
			if opts.SessionMgr == nil {
				http.NotFound(w, req)
				return
			}
			opts.SessionMgr.HandleSessionLogout(w, req)
		case req.URL.Path == "/v1/admin/session/me":
			if opts.SessionMgr == nil {
				http.NotFound(w, req)
				return
			}
			opts.SessionMgr.HandleSessionMe(w, req)
		case req.URL.Path == "/v1/admin/tokens":
			handleTokens(w, req, opts)
		case revokeTarget(req.URL.Path) != "":
			handleRevokeToken(w, req, opts)
		case req.URL.Path == "/v1/admin/audit/summary":
			handleAuditSummary(w, req, opts)
		case req.URL.Path == "/v1/admin/audit/events":
			handleAuditEvents(w, req, opts)
		case req.URL.Path == "/v1/admin/dashboard":
			handleDashboard(w, req, opts)
		case req.URL.Path == "/v1/admin/billing/summary":
			handleBillingSummary(w, req, opts)
		case req.URL.Path == "/v1/admin/billing/periods":
			handleBillingPeriods(w, req, opts)
		case req.URL.Path == "/v1/admin/billing/alerts":
			handleBillingAlerts(w, req, opts)
		case req.URL.Path == "/v1/admin/usage/summary":
			handleUsageSummary(w, req, opts)
		case req.URL.Path == "/v1/admin/usage/events":
			handleUsageEvents(w, req, opts)
		case req.URL.Path == "/v1/admin/users":
			handleUsers(w, req, opts)
		case req.URL.Path == "/v1/admin/organizations":
			handleOrganizations(w, req, opts)
		case req.URL.Path == "/v1/admin/projects":
			handleProjects(w, req, opts)
		case req.URL.Path == "/v1/admin/organization-memberships":
			handleOrganizationMemberships(w, req, opts)
		case req.URL.Path == "/v1/admin/project-memberships":
			handleProjectMemberships(w, req, opts)
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
		items := filterTokenViews(opts.TokenManager.TokenRules(), req, grant)
		page := paginateBounds(len(items), req)
		writeJSON(w, http.StatusOK, map[string]any{
			"data":       items[page.Start:page.End],
			"pagination": page.Meta(len(items)),
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
	dec := newLimitedJSONDecoder(w, req)
	if err := dec.Decode(&payload); err != nil {
		status, code, message := adminJSONDecodeError(err)
		writeError(w, status, code, message)
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", status, message, 0, 0, 0)
		return
	}
	rule := serverauth.TokenRule{
		ID:                 strings.TrimSpace(payload.ID),
		Token:              strings.TrimSpace(payload.Token),
		Description:        strings.TrimSpace(payload.Description),
		UserID:             strings.TrimSpace(payload.UserID),
		OrganizationID:     strings.TrimSpace(payload.OrganizationID),
		ProjectID:          strings.TrimSpace(payload.ProjectID),
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
	if err := enforceGrantTokenScope(grant, &rule); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", http.StatusForbidden, err.Error(), 0, 0, 0)
		return
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
	if err := enforceGrantRevokeScope(opts.TokenManager, grant, tokenID); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminTokensWrite, "admin_tokens", http.StatusForbidden, err.Error(), 0, 0, 0)
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
	allowedTokens := tenantTokenIDs(opts.TokenManager, grant)
	if err := checkAuditTokenFilter(filter.TokenID, allowedTokens); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusForbidden, err.Error(), 0, 0, 0)
		return
	}
	events, err := loadAuditEvents(opts.AuditPath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	events = filterAuditEventsByTenant(events, allowedTokens)
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
	allowedTokens := tenantTokenIDs(opts.TokenManager, grant)
	if err := checkAuditTokenFilter(filter.TokenID, allowedTokens); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusForbidden, err.Error(), 0, 0, 0)
		return
	}
	events, err := loadAuditEvents(opts.AuditPath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	events = filterAuditEventsByTenant(events, allowedTokens)
	if wantsCSV(req) {
		writeCSVHeaders(w, "audit-events.csv")
		if err := serveraudit.WriteEventsCSV(w, events); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusInternalServerError, err.Error(), 0, 0, 0)
			return
		}
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminAuditRead, "admin_audit", http.StatusOK, "", 0, 0, 0)
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
	constrainUsageFilterByGrant(&filter, grant)
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
	constrainUsageFilterByGrant(&filter, grant)
	entries, err := loadUsageEntries(opts.UsageStore, opts.UsagePath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	if wantsCSV(req) {
		writeCSVHeaders(w, "usage-events.csv")
		if err := serverusage.WriteEntriesCSV(w, entries); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusInternalServerError, err.Error(), 0, 0, 0)
			return
		}
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_usage", http.StatusOK, "", 0, 0, 0)
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
	users = filterUsers(users, req, grant)
	page := paginateBounds(len(users), req)
	writeJSON(w, http.StatusOK, map[string]any{
		"data":       users[page.Start:page.End],
		"pagination": page.Meta(len(users)),
	})
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
	if scopedUserID := grant.UserID(); scopedUserID != "" && userID != scopedUserID {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("scoped admin tokens may only modify user %q", scopedUserID))
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_users", http.StatusForbidden, fmt.Sprintf("scoped admin tokens may only modify user %q", scopedUserID), 0, 0, 0)
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
		dec := newLimitedJSONDecoder(w, req)
		if decodeErr := dec.Decode(&payload); decodeErr != nil {
			status, code, message := adminJSONDecodeError(decodeErr)
			writeError(w, status, code, message)
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_users", status, message, 0, 0, 0)
			return
		}
		user, err = opts.UserStore.SetUserRole(userID, payload.Role)
	case "password":
		var payload updateUserPasswordRequest
		dec := newLimitedJSONDecoder(w, req)
		if decodeErr := dec.Decode(&payload); decodeErr != nil {
			status, code, message := adminJSONDecodeError(decodeErr)
			writeError(w, status, code, message)
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_users", status, message, 0, 0, 0)
			return
		}
		user, err = opts.UserStore.SetUserPassword(userID, payload.Password)
	default:
		http.NotFound(w, req)
		return
	}
	if err != nil {
		var status int
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
	if opts.TokenManager == nil || (opts.Authorizer == nil && opts.SessionMgr == nil) {
		writeError(w, http.StatusNotFound, "not_found", "admin API is unavailable without auth")
		logAdminEvent(opts.AuditLogger, req, nil, scope, kind, http.StatusNotFound, "admin API is unavailable without --auth-config", 0, 0, 0)
		return nil, false
	}
	var (
		grant   *serverauth.Grant
		ok      bool
		session *AdminSession
	)
	if opts.Authorizer != nil {
		grant, ok = opts.Authorizer.AuthenticateRequest(req)
	}
	if !ok && opts.SessionMgr != nil {
		grant, session, ok = opts.SessionMgr.Authenticate(req)
	}
	if !ok || grant == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing admin credentials")
		logAdminEvent(opts.AuditLogger, req, nil, scope, kind, http.StatusUnauthorized, "invalid or missing admin credentials", 0, 0, 0)
		return nil, false
	}
	if session != nil && requiresCSRFAuthorization(req.Method) && !opts.SessionMgr.ValidateCSRF(req, session.CSRFToken) {
		writeError(w, http.StatusForbidden, "forbidden", "missing or invalid CSRF token")
		logAdminEvent(opts.AuditLogger, req, grant, scope, kind, http.StatusForbidden, "missing or invalid CSRF token", 0, 0, 0)
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

type pageWindow struct {
	Limit  int
	Offset int
	Start  int
	End    int
}

func paginateBounds(total int, req *http.Request) pageWindow {
	limit := 100
	offset := 0
	if req != nil {
		query := req.URL.Query()
		if value, err := parseOptionalInt(query.Get("limit")); err == nil && value > 0 {
			limit = value
		}
		if value, err := parseOptionalInt(query.Get("offset")); err == nil && value > 0 {
			offset = value
		}
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return pageWindow{Limit: limit, Offset: offset, Start: offset, End: end}
}

func (p pageWindow) Meta(total int) map[string]any {
	return map[string]any{
		"total":    total,
		"limit":    p.Limit,
		"offset":   p.Offset,
		"returned": p.End - p.Start,
	}
}

func constrainUsageFilterByGrant(filter *serverusage.Filter, grant *serverauth.Grant) {
	if filter == nil || grant == nil {
		return
	}
	if grant.OrganizationID() != "" {
		filter.OrgID = grant.OrganizationID()
	}
	if grant.ProjectID() != "" {
		filter.ProjectID = grant.ProjectID()
	}
	if grant.UserID() != "" {
		filter.UserID = grant.UserID()
	}
}

// enforceGrantTokenScope constrains a child token rule to the issuing grant.
// The child's tenant (user/org/project), scopes, expiry, workspace/provider/mode
// allowlists, and quotas must all be a subset of (⊆) the issuer's. Attempts to
// grant beyond the issuer are rejected with a clear error rather than silently
// widened. An unrestricted issuer (root admin: no expiry, empty allowlists,
// zero quotas) imposes no ceiling on those dimensions, so ordinary issuance is
// unaffected.
func enforceGrantTokenScope(grant *serverauth.Grant, rule *serverauth.TokenRule) error {
	if grant == nil || rule == nil {
		return nil
	}
	if scopedUserID := grant.UserID(); scopedUserID != "" {
		if strings.TrimSpace(rule.UserID) == "" {
			rule.UserID = scopedUserID
		} else if strings.TrimSpace(rule.UserID) != scopedUserID {
			return fmt.Errorf("scoped admin tokens may only issue tokens for user %q", scopedUserID)
		}
	}
	if scopedOrgID := grant.OrganizationID(); scopedOrgID != "" {
		if strings.TrimSpace(rule.OrganizationID) == "" {
			rule.OrganizationID = scopedOrgID
		} else if strings.TrimSpace(rule.OrganizationID) != scopedOrgID {
			return fmt.Errorf("scoped admin tokens may only issue tokens for organization %q", scopedOrgID)
		}
	}
	if scopedProjectID := grant.ProjectID(); scopedProjectID != "" {
		if strings.TrimSpace(rule.ProjectID) == "" {
			rule.ProjectID = scopedProjectID
		} else if strings.TrimSpace(rule.ProjectID) != scopedProjectID {
			return fmt.Errorf("scoped admin tokens may only issue tokens for project %q", scopedProjectID)
		}
	}

	if allowed := stringSet(grant.Scopes()); len(allowed) > 0 {
		for _, scope := range rule.Scopes {
			norm := strings.ToLower(strings.TrimSpace(scope))
			if norm == "" {
				continue
			}
			if _, ok := allowed[norm]; !ok {
				return fmt.Errorf("issued token scope %q exceeds issuer permissions", norm)
			}
		}
	}

	if issuerExpiry := grant.ExpiresAt(); !issuerExpiry.IsZero() {
		if rule.ExpiresAt.IsZero() {
			return fmt.Errorf("issued token must expire no later than the issuing token (%s)", issuerExpiry.UTC().Format(time.RFC3339))
		}
		if rule.ExpiresAt.After(issuerExpiry) {
			return fmt.Errorf("issued token expiry %s exceeds issuing token expiry %s", rule.ExpiresAt.UTC().Format(time.RFC3339), issuerExpiry.UTC().Format(time.RFC3339))
		}
	}

	if err := enforcePrefixSubset(grant.WorkspacePrefixes(), rule.WorkspacePrefixes); err != nil {
		return err
	}
	if err := enforceAllowlistSubset("provider", grant.AllowedProviders(), rule.AllowedProviders); err != nil {
		return err
	}
	if err := enforceAllowlistSubset("mode", grant.AllowedModes(), rule.AllowedModes); err != nil {
		return err
	}

	if err := enforceQuotaCeiling("max_requests_per_hour", grant.MaxRequestsPerHour(), rule.MaxRequestsPerHour); err != nil {
		return err
	}
	if err := enforceQuotaCeiling("max_requests_per_day", grant.MaxRequestsPerDay(), rule.MaxRequestsPerDay); err != nil {
		return err
	}
	if err := enforceCostCeiling("max_cost_usd_per_day", grant.MaxCostUSDPerDay(), rule.MaxCostUSDPerDay); err != nil {
		return err
	}
	if err := enforceCostCeiling("max_cost_usd_per_month", grant.MaxCostUSDPerMonth(), rule.MaxCostUSDPerMonth); err != nil {
		return err
	}
	return nil
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		out[value] = struct{}{}
	}
	return out
}

// enforcePrefixSubset requires every child workspace prefix to be covered by an
// issuer prefix (i.e. equal to or narrower than one of them). When the issuer
// restricts prefixes, the child must too. The child's prefixes are normalized
// (trimmed, blanks dropped) exactly as newGrant will normalize them BEFORE the
// subset check, so a request of only whitespace/blank entries collapses to an
// empty (=unrestricted) allowlist and is rejected rather than silently widening
// access beyond the issuer.
func enforcePrefixSubset(issuer, child []string) error {
	if len(issuer) == 0 {
		return nil
	}
	effective := normalizePrefixList(child)
	if len(effective) == 0 {
		return fmt.Errorf("issued token must restrict workspace_prefixes to a subset of the issuing token")
	}
	for _, prefix := range effective {
		covered := false
		for _, allowed := range issuer {
			if strings.HasPrefix(prefix, allowed) {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("issued workspace prefix %q exceeds issuer permissions", prefix)
		}
	}
	return nil
}

// enforceAllowlistSubset requires the child allowlist to be a non-empty subset
// of the issuer's when the issuer restricts the dimension. The child allowlist
// is normalized (lowercased, trimmed, blanks dropped) exactly as newGrant will
// normalize it BEFORE the subset check, so a request of only whitespace/blank
// entries collapses to an empty (=unrestricted) allowlist and is rejected
// rather than silently widening access beyond the issuer.
func enforceAllowlistSubset(kind string, issuer, child []string) error {
	allowed := stringSet(issuer)
	if len(allowed) == 0 {
		return nil
	}
	effective := normalizeAllowlist(child)
	if len(effective) == 0 {
		return fmt.Errorf("issued token must restrict allowed %ss to a subset of the issuing token", kind)
	}
	for _, value := range effective {
		if _, ok := allowed[value]; !ok {
			return fmt.Errorf("issued %s %q exceeds issuer permissions", kind, value)
		}
	}
	return nil
}

// normalizePrefixList trims and drops blank workspace prefixes, matching the
// normalization newGrant applies (prefixes are not lowercased). Order is
// preserved for deterministic error messages.
func normalizePrefixList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

// normalizeAllowlist lowercases, trims, and drops blank entries, matching the
// normalization newGrant applies to provider/mode allowlists. Order is
// preserved for deterministic error messages.
func normalizeAllowlist(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

// enforceQuotaCeiling requires a positive child quota that does not exceed a
// positive issuer quota. An issuer value of 0 means unlimited and imposes no
// ceiling.
func enforceQuotaCeiling(field string, issuer, child int) error {
	if issuer <= 0 {
		return nil
	}
	if child <= 0 || child > issuer {
		return fmt.Errorf("issued token %s must be between 1 and the issuer limit %d", field, issuer)
	}
	return nil
}

func enforceCostCeiling(field string, issuer, child float64) error {
	if issuer <= 0 {
		return nil
	}
	if child <= 0 || child > issuer {
		return fmt.Errorf("issued token %s must be between 0 and the issuer limit %g", field, issuer)
	}
	return nil
}

// tenantTokenIDs returns the set of token IDs visible to a tenant-scoped grant,
// or nil when the grant is not tenant-scoped (an unrestricted admin). The
// caller's own token is always included.
func tenantTokenIDs(tm serverauth.TokenManager, grant *serverauth.Grant) map[string]struct{} {
	if grant == nil {
		return nil
	}
	userID := grant.UserID()
	orgID := grant.OrganizationID()
	projectID := grant.ProjectID()
	if userID == "" && orgID == "" && projectID == "" {
		return nil
	}
	ids := make(map[string]struct{})
	if tm != nil {
		for _, view := range tm.TokenRules() {
			if userID != "" && view.UserID != userID {
				continue
			}
			if orgID != "" && view.OrganizationID != orgID {
				continue
			}
			if projectID != "" && view.ProjectID != projectID {
				continue
			}
			ids[view.ID] = struct{}{}
		}
	}
	if id := grant.TokenID(); id != "" {
		ids[id] = struct{}{}
	}
	return ids
}

// enforceGrantRevokeScope prevents a tenant-scoped admin from revoking tokens
// outside its tenant.
func enforceGrantRevokeScope(tm serverauth.TokenManager, grant *serverauth.Grant, tokenID string) error {
	allowed := tenantTokenIDs(tm, grant)
	if allowed == nil {
		return nil
	}
	if _, ok := allowed[strings.TrimSpace(tokenID)]; !ok {
		return fmt.Errorf("scoped admin tokens may only revoke tokens within their tenant")
	}
	return nil
}

// checkAuditTokenFilter rejects an explicit token_id filter that targets a
// token outside the caller's tenant.
func checkAuditTokenFilter(tokenID string, allowed map[string]struct{}) error {
	if allowed == nil {
		return nil
	}
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return nil
	}
	if _, ok := allowed[tokenID]; !ok {
		return fmt.Errorf("scoped admin tokens may only read audit events within their tenant")
	}
	return nil
}

// filterAuditEventsByTenant drops audit events whose token is outside the
// tenant-scoped set. A nil set means the caller is unrestricted.
func filterAuditEventsByTenant(events []serveraudit.Event, allowed map[string]struct{}) []serveraudit.Event {
	if allowed == nil {
		return events
	}
	out := events[:0]
	for _, event := range events {
		if _, ok := allowed[event.TokenID]; ok {
			out = append(out, event)
		}
	}
	return out
}

func filterTokenViews(items []serverauth.TokenRuleView, req *http.Request, grant *serverauth.Grant) []serverauth.TokenRuleView {
	if len(items) == 0 {
		return nil
	}
	query := req.URL.Query()
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	userID := strings.TrimSpace(query.Get("user_id"))
	orgID := strings.TrimSpace(query.Get("organization_id"))
	projectID := strings.TrimSpace(query.Get("project_id"))
	revokedFilter := strings.TrimSpace(query.Get("revoked"))
	if grant != nil {
		if scopedUserID := grant.UserID(); scopedUserID != "" {
			userID = scopedUserID
		}
		if scopedOrgID := grant.OrganizationID(); scopedOrgID != "" {
			orgID = scopedOrgID
		}
		if scopedProjectID := grant.ProjectID(); scopedProjectID != "" {
			projectID = scopedProjectID
		}
	}
	out := make([]serverauth.TokenRuleView, 0, len(items))
	for _, item := range items {
		if userID != "" && item.UserID != userID {
			continue
		}
		if orgID != "" && item.OrganizationID != orgID {
			continue
		}
		if projectID != "" && item.ProjectID != projectID {
			continue
		}
		if revokedFilter != "" && strings.EqualFold(revokedFilter, "true") != item.Revoked {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(strings.Join([]string{
				item.ID, item.Description, item.UserID, item.OrganizationID, item.ProjectID, strings.Join(item.Scopes, ","),
			}, " "))
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func filterUsers(items []router.UserView, req *http.Request, grant *serverauth.Grant) []router.UserView {
	if len(items) == 0 {
		return nil
	}
	query := req.URL.Query()
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	role := strings.ToLower(strings.TrimSpace(query.Get("role")))
	activeFilter := strings.TrimSpace(query.Get("active"))
	userID := ""
	if grant != nil {
		userID = grant.UserID()
	}
	out := make([]router.UserView, 0, len(items))
	for _, item := range items {
		if userID != "" && item.ID != userID {
			continue
		}
		if role != "" && strings.ToLower(item.Role) != role {
			continue
		}
		if activeFilter != "" && strings.EqualFold(activeFilter, "true") != item.IsActive {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(item.ID + " " + item.Email + " " + item.Role)
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
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
		UserID:     strings.TrimSpace(query.Get("user_id")),
		OrgID:      strings.TrimSpace(query.Get("organization_id")),
		ProjectID:  strings.TrimSpace(query.Get("project_id")),
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
	case "activate", "deactivate", "role", "password":
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

func newLimitedJSONDecoder(w http.ResponseWriter, req *http.Request) *json.Decoder {
	req.Body = http.MaxBytesReader(w, req.Body, maxAdminJSONBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	return dec
}

func adminJSONDecodeError(err error) (int, string, string) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request body exceeds %d bytes limit", maxErr.Limit)
	}
	return http.StatusBadRequest, "invalid_request", "invalid JSON: " + err.Error()
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
