package serveradmin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

type organizationCreateRequest struct {
	ID               string  `json:"id,omitempty"`
	Name             string  `json:"name"`
	Slug             string  `json:"slug,omitempty"`
	Description      string  `json:"description,omitempty"`
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd,omitempty"`
}

type projectCreateRequest struct {
	ID               string  `json:"id,omitempty"`
	OrganizationID   string  `json:"organization_id"`
	Name             string  `json:"name"`
	Slug             string  `json:"slug,omitempty"`
	Description      string  `json:"description,omitempty"`
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd,omitempty"`
}

type organizationMembershipRequest struct {
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Role           string `json:"role,omitempty"`
	IsActive       *bool  `json:"is_active,omitempty"`
}

type projectMembershipRequest struct {
	ProjectID string `json:"project_id"`
	UserID    string `json:"user_id"`
	Role      string `json:"role,omitempty"`
	IsActive  *bool  `json:"is_active,omitempty"`
}

func handleOrganizations(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if opts.TeamStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "organization management is unavailable")
		return
	}
	switch req.Method {
	case http.MethodGet:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersRead, "admin_organizations")
		if !ok {
			return
		}
		items, err := opts.TeamStore.ListOrganizations()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_organizations", http.StatusInternalServerError, err.Error(), 0, 0, 0)
			return
		}
		items = filterOrganizations(items, req, grant)
		page := paginateBounds(len(items), req)
		writeJSON(w, http.StatusOK, map[string]any{
			"data":       items[page.Start:page.End],
			"pagination": page.Meta(len(items)),
		})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_organizations", http.StatusOK, "", 0, 0, 0)
	case http.MethodPost:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersWrite, "admin_organizations")
		if !ok {
			return
		}
		var payload organizationCreateRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organizations", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		if grant != nil && grant.OrganizationID() != "" {
			writeError(w, http.StatusForbidden, "forbidden", "scoped admin tokens cannot create additional organizations")
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organizations", http.StatusForbidden, "scoped admin tokens cannot create additional organizations", 0, 0, 0)
			return
		}
		item, err := opts.TeamStore.CreateOrganization(serverteam.Organization{
			ID:               strings.TrimSpace(payload.ID),
			Name:             strings.TrimSpace(payload.Name),
			Slug:             strings.TrimSpace(payload.Slug),
			Description:      strings.TrimSpace(payload.Description),
			MonthlyBudgetUSD: payload.MonthlyBudgetUSD,
			IsActive:         true,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organizations", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"organization": item})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organizations", http.StatusCreated, "", 0, 0, 0)
	default:
		writeMethodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

func handleProjects(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if opts.TeamStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "project management is unavailable")
		return
	}
	switch req.Method {
	case http.MethodGet:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersRead, "admin_projects")
		if !ok {
			return
		}
		orgID := strings.TrimSpace(req.URL.Query().Get("organization_id"))
		if grant != nil && grant.OrganizationID() != "" {
			orgID = grant.OrganizationID()
		}
		items, err := opts.TeamStore.ListProjects(orgID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_projects", http.StatusInternalServerError, err.Error(), 0, 0, 0)
			return
		}
		items = filterProjects(items, req, grant)
		page := paginateBounds(len(items), req)
		writeJSON(w, http.StatusOK, map[string]any{
			"data":       items[page.Start:page.End],
			"pagination": page.Meta(len(items)),
		})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_projects", http.StatusOK, "", 0, 0, 0)
	case http.MethodPost:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersWrite, "admin_projects")
		if !ok {
			return
		}
		var payload projectCreateRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_projects", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		if grant != nil && grant.OrganizationID() != "" {
			if strings.TrimSpace(payload.OrganizationID) == "" {
				payload.OrganizationID = grant.OrganizationID()
			} else if strings.TrimSpace(payload.OrganizationID) != grant.OrganizationID() {
				writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("scoped admin tokens may only manage organization %q", grant.OrganizationID()))
				logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_projects", http.StatusForbidden, fmt.Sprintf("scoped admin tokens may only manage organization %q", grant.OrganizationID()), 0, 0, 0)
				return
			}
		}
		if grant != nil && grant.ProjectID() != "" {
			writeError(w, http.StatusForbidden, "forbidden", "scoped project admin tokens cannot create additional projects")
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_projects", http.StatusForbidden, "scoped project admin tokens cannot create additional projects", 0, 0, 0)
			return
		}
		item, err := opts.TeamStore.CreateProject(serverteam.Project{
			ID:               strings.TrimSpace(payload.ID),
			OrganizationID:   strings.TrimSpace(payload.OrganizationID),
			Name:             strings.TrimSpace(payload.Name),
			Slug:             strings.TrimSpace(payload.Slug),
			Description:      strings.TrimSpace(payload.Description),
			MonthlyBudgetUSD: payload.MonthlyBudgetUSD,
			IsActive:         true,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_projects", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"project": item})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_projects", http.StatusCreated, "", 0, 0, 0)
	default:
		writeMethodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

func handleBillingSummary(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsageRead, "admin_billing")
	if !ok {
		return
	}
	filter, err := usageFilterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	constrainUsageFilterByGrant(&filter, grant)
	entries, err := loadUsageEntries(opts.UsageStore, opts.UsagePath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	summary := serverusage.SummarizeEntries(entries)
	billing := serverteam.BillingSummary{}
	if opts.TeamStore != nil {
		if orgs, err := opts.TeamStore.ListOrganizations(); err == nil {
			orgs = filterOrganizations(orgs, req, grant)
			for _, org := range orgs {
				billing.Organizations = append(billing.Organizations, newBillingBucket(
					org.ID,
					org.Name,
					org.MonthlyBudgetUSD,
					summary.CostByOrganization[org.ID],
					summary.ByOrganization[org.ID],
				))
			}
		}
		orgID := strings.TrimSpace(req.URL.Query().Get("organization_id"))
		if grant != nil && grant.OrganizationID() != "" {
			orgID = grant.OrganizationID()
		}
		if projects, err := opts.TeamStore.ListProjects(orgID); err == nil {
			projects = filterProjects(projects, req, grant)
			for _, project := range projects {
				billing.Projects = append(billing.Projects, newBillingBucket(
					project.ID,
					project.Name,
					project.MonthlyBudgetUSD,
					summary.CostByProject[project.ID],
					summary.ByProject[project.ID],
				))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    opts.UsagePath,
		"usage":   summary,
		"billing": billing,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing", http.StatusOK, "", 0, 0, 0)
}

func handleBillingPeriods(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsageRead, "admin_billing_periods")
	if !ok {
		return
	}
	filter, err := usageFilterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing_periods", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	constrainUsageFilterByGrant(&filter, grant)
	entries, err := loadUsageEntries(opts.UsageStore, opts.UsagePath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing_periods", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    opts.UsagePath,
		"periods": serverusage.SummarizeMonthlyPeriods(entries),
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing_periods", http.StatusOK, "", 0, 0, 0)
}

func handleBillingAlerts(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsageRead, "admin_billing_alerts")
	if !ok {
		return
	}
	filter, err := usageFilterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing_alerts", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	constrainUsageFilterByGrant(&filter, grant)
	entries, err := loadUsageEntries(opts.UsageStore, opts.UsagePath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing_alerts", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	summary := serverusage.SummarizeEntries(entries)
	alerts := make([]serverteam.BudgetAlert, 0, 8)
	if opts.TeamStore != nil {
		if orgs, err := opts.TeamStore.ListOrganizations(); err == nil {
			orgs = filterOrganizations(orgs, req, grant)
			for _, org := range orgs {
				if alert, ok := billingAlertFromBucket("organization", newBillingBucket(
					org.ID,
					org.Name,
					org.MonthlyBudgetUSD,
					summary.CostByOrganization[org.ID],
					summary.ByOrganization[org.ID],
				)); ok {
					alerts = append(alerts, alert)
				}
			}
		}
		orgID := strings.TrimSpace(req.URL.Query().Get("organization_id"))
		if grant != nil && grant.OrganizationID() != "" {
			orgID = grant.OrganizationID()
		}
		if projects, err := opts.TeamStore.ListProjects(orgID); err == nil {
			projects = filterProjects(projects, req, grant)
			for _, project := range projects {
				if alert, ok := billingAlertFromBucket("project", newBillingBucket(
					project.ID,
					project.Name,
					project.MonthlyBudgetUSD,
					summary.CostByProject[project.ID],
					summary.ByProject[project.ID],
				)); ok {
					alerts = append(alerts, alert)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":   opts.UsagePath,
		"alerts": alerts,
	})
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_billing_alerts", http.StatusOK, "", 0, 0, 0)
}

func handleDashboard(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsageRead, "admin_dashboard")
	if !ok {
		return
	}
	filter, err := usageFilterFromQuery(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_dashboard", http.StatusBadRequest, err.Error(), 0, 0, 0)
		return
	}
	constrainUsageFilterByGrant(&filter, grant)
	entries, err := loadUsageEntries(opts.UsageStore, opts.UsagePath, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_dashboard", http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return
	}
	payload := map[string]any{
		"usage": map[string]any{
			"path":    opts.UsagePath,
			"summary": serverusage.SummarizeEntries(entries),
		},
	}
	if opts.TokenManager != nil && grant.AllowsScope(serverauth.ScopeAdminTokensRead) {
		payload["tokens"] = map[string]any{
			"count": len(opts.TokenManager.TokenRules()),
			"data":  opts.TokenManager.TokenRules(),
		}
	}
	if opts.UserStore != nil && grant.AllowsScope(serverauth.ScopeAdminUsersRead) {
		if users, err := opts.UserStore.ListUsers(); err == nil {
			payload["users"] = map[string]any{
				"count": len(users),
				"data":  users,
			}
		}
	}
	if opts.TeamStore != nil && grant.AllowsScope(serverauth.ScopeAdminUsersRead) {
		if orgs, err := opts.TeamStore.ListOrganizations(); err == nil {
			orgs = filterOrganizations(orgs, req, grant)
			payload["organizations"] = map[string]any{
				"count": len(orgs),
				"data":  orgs,
			}
		}
		orgID := ""
		if grant.OrganizationID() != "" {
			orgID = grant.OrganizationID()
		}
		if projects, err := opts.TeamStore.ListProjects(orgID); err == nil {
			projects = filterProjects(projects, req, grant)
			payload["projects"] = map[string]any{
				"count": len(projects),
				"data":  projects,
			}
		}
	}
	writeJSON(w, http.StatusOK, payload)
	logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsageRead, "admin_dashboard", http.StatusOK, "", 0, 0, 0)
}

func handleOrganizationMemberships(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if opts.TeamStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "organization membership management is unavailable")
		return
	}
	switch req.Method {
	case http.MethodGet:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersRead, "admin_organization_memberships")
		if !ok {
			return
		}
		orgID := strings.TrimSpace(req.URL.Query().Get("organization_id"))
		userID := strings.TrimSpace(req.URL.Query().Get("user_id"))
		if grant != nil && grant.OrganizationID() != "" {
			orgID = grant.OrganizationID()
		}
		items, err := opts.TeamStore.ListOrganizationMemberships(orgID, userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_organization_memberships", http.StatusInternalServerError, err.Error(), 0, 0, 0)
			return
		}
		items = filterOrganizationMemberships(items, req, grant)
		page := paginateBounds(len(items), req)
		writeJSON(w, http.StatusOK, map[string]any{
			"data":       items[page.Start:page.End],
			"pagination": page.Meta(len(items)),
		})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_organization_memberships", http.StatusOK, "", 0, 0, 0)
	case http.MethodPost:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersWrite, "admin_organization_memberships")
		if !ok {
			return
		}
		var payload organizationMembershipRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organization_memberships", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		if grant != nil && grant.ProjectID() != "" {
			writeError(w, http.StatusForbidden, "forbidden", "project-scoped admin tokens cannot manage organization memberships")
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organization_memberships", http.StatusForbidden, "project-scoped admin tokens cannot manage organization memberships", 0, 0, 0)
			return
		}
		if grant != nil && grant.OrganizationID() != "" {
			if strings.TrimSpace(payload.OrganizationID) == "" {
				payload.OrganizationID = grant.OrganizationID()
			} else if strings.TrimSpace(payload.OrganizationID) != grant.OrganizationID() {
				writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("scoped admin tokens may only manage organization %q", grant.OrganizationID()))
				logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organization_memberships", http.StatusForbidden, fmt.Sprintf("scoped admin tokens may only manage organization %q", grant.OrganizationID()), 0, 0, 0)
				return
			}
		}
		active := true
		if payload.IsActive != nil {
			active = *payload.IsActive
		}
		item, err := opts.TeamStore.UpsertOrganizationMembership(serverteam.OrganizationMembership{
			OrganizationID: strings.TrimSpace(payload.OrganizationID),
			UserID:         strings.TrimSpace(payload.UserID),
			Role:           strings.TrimSpace(payload.Role),
			IsActive:       active,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organization_memberships", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"membership": item})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_organization_memberships", http.StatusCreated, "", 0, 0, 0)
	default:
		writeMethodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

func handleProjectMemberships(w http.ResponseWriter, req *http.Request, opts HandlerOptions) {
	if opts.TeamStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "project membership management is unavailable")
		return
	}
	switch req.Method {
	case http.MethodGet:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersRead, "admin_project_memberships")
		if !ok {
			return
		}
		projectID := strings.TrimSpace(req.URL.Query().Get("project_id"))
		userID := strings.TrimSpace(req.URL.Query().Get("user_id"))
		if grant != nil && grant.ProjectID() != "" {
			projectID = grant.ProjectID()
		}
		items, err := opts.TeamStore.ListProjectMemberships(projectID, userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_project_memberships", http.StatusInternalServerError, err.Error(), 0, 0, 0)
			return
		}
		items = filterProjectMemberships(items, req, grant)
		page := paginateBounds(len(items), req)
		writeJSON(w, http.StatusOK, map[string]any{
			"data":       items[page.Start:page.End],
			"pagination": page.Meta(len(items)),
		})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersRead, "admin_project_memberships", http.StatusOK, "", 0, 0, 0)
	case http.MethodPost:
		grant, ok := authenticateAdmin(w, req, opts, serverauth.ScopeAdminUsersWrite, "admin_project_memberships")
		if !ok {
			return
		}
		var payload projectMembershipRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_project_memberships", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		if grant != nil && grant.ProjectID() != "" && strings.TrimSpace(payload.ProjectID) != "" && strings.TrimSpace(payload.ProjectID) != grant.ProjectID() {
			writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("scoped admin tokens may only manage project %q", grant.ProjectID()))
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_project_memberships", http.StatusForbidden, fmt.Sprintf("scoped admin tokens may only manage project %q", grant.ProjectID()), 0, 0, 0)
			return
		}
		if grant != nil && grant.OrganizationID() != "" {
			project, err := opts.TeamStore.GetProject(strings.TrimSpace(payload.ProjectID))
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_project_memberships", http.StatusBadRequest, err.Error(), 0, 0, 0)
				return
			}
			if project.OrganizationID != grant.OrganizationID() {
				writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("scoped admin tokens may only manage organization %q", grant.OrganizationID()))
				logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_project_memberships", http.StatusForbidden, fmt.Sprintf("scoped admin tokens may only manage organization %q", grant.OrganizationID()), 0, 0, 0)
				return
			}
		}
		active := true
		if payload.IsActive != nil {
			active = *payload.IsActive
		}
		item, err := opts.TeamStore.UpsertProjectMembership(serverteam.ProjectMembership{
			ProjectID: strings.TrimSpace(payload.ProjectID),
			UserID:    strings.TrimSpace(payload.UserID),
			Role:      strings.TrimSpace(payload.Role),
			IsActive:  active,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_project_memberships", http.StatusBadRequest, err.Error(), 0, 0, 0)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"membership": item})
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_project_memberships", http.StatusCreated, "", 0, 0, 0)
	default:
		writeMethodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

func filterOrganizations(items []serverteam.Organization, req *http.Request, grant *serverauth.Grant) []serverteam.Organization {
	if len(items) == 0 {
		return nil
	}
	query := req.URL.Query()
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	scopedOrgID := ""
	if grant != nil {
		scopedOrgID = grant.OrganizationID()
	}
	out := make([]serverteam.Organization, 0, len(items))
	for _, item := range items {
		if scopedOrgID != "" && item.ID != scopedOrgID {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(item.ID + " " + item.Name + " " + item.Slug + " " + item.Description)
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func filterProjects(items []serverteam.Project, req *http.Request, grant *serverauth.Grant) []serverteam.Project {
	if len(items) == 0 {
		return nil
	}
	query := req.URL.Query()
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	orgID := strings.TrimSpace(query.Get("organization_id"))
	projectID := ""
	if grant != nil {
		if grant.OrganizationID() != "" {
			orgID = grant.OrganizationID()
		}
		if grant.ProjectID() != "" {
			projectID = grant.ProjectID()
		}
	}
	out := make([]serverteam.Project, 0, len(items))
	for _, item := range items {
		if orgID != "" && item.OrganizationID != orgID {
			continue
		}
		if projectID != "" && item.ID != projectID {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(item.ID + " " + item.OrganizationID + " " + item.Name + " " + item.Slug + " " + item.Description)
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func filterOrganizationMemberships(items []serverteam.OrganizationMembership, req *http.Request, grant *serverauth.Grant) []serverteam.OrganizationMembership {
	if len(items) == 0 {
		return nil
	}
	query := req.URL.Query()
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	orgID := strings.TrimSpace(query.Get("organization_id"))
	userID := strings.TrimSpace(query.Get("user_id"))
	activeFilter := strings.TrimSpace(query.Get("active"))
	if grant != nil && grant.OrganizationID() != "" {
		orgID = grant.OrganizationID()
	}
	out := make([]serverteam.OrganizationMembership, 0, len(items))
	for _, item := range items {
		if orgID != "" && item.OrganizationID != orgID {
			continue
		}
		if userID != "" && item.UserID != userID {
			continue
		}
		if activeFilter != "" && strings.EqualFold(activeFilter, "true") != item.IsActive {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(item.OrganizationID + " " + item.UserID + " " + item.Role)
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func filterProjectMemberships(items []serverteam.ProjectMembership, req *http.Request, grant *serverauth.Grant) []serverteam.ProjectMembership {
	if len(items) == 0 {
		return nil
	}
	query := req.URL.Query()
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	projectID := strings.TrimSpace(query.Get("project_id"))
	userID := strings.TrimSpace(query.Get("user_id"))
	activeFilter := strings.TrimSpace(query.Get("active"))
	if grant != nil {
		if grant.OrganizationID() != "" {
			// grant-scoped org is enforced via OrganizationID on each membership.
		}
		if grant.ProjectID() != "" {
			projectID = grant.ProjectID()
		}
	}
	out := make([]serverteam.ProjectMembership, 0, len(items))
	for _, item := range items {
		if grant != nil && grant.OrganizationID() != "" && item.OrganizationID != grant.OrganizationID() {
			continue
		}
		if projectID != "" && item.ProjectID != projectID {
			continue
		}
		if userID != "" && item.UserID != userID {
			continue
		}
		if activeFilter != "" && strings.EqualFold(activeFilter, "true") != item.IsActive {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(item.ProjectID + " " + item.OrganizationID + " " + item.UserID + " " + item.Role)
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func newBillingBucket(id, name string, budgetUSD, spendUSD float64, requestCount int) serverteam.BillingBucket {
	bucket := serverteam.BillingBucket{
		ID:                 id,
		Name:               name,
		MonthlyBudgetUSD:   budgetUSD,
		SpendUSD:           spendUSD,
		RemainingBudgetUSD: budgetUSD - spendUSD,
		RequestCount:       requestCount,
		OverBudget:         budgetUSD > 0 && spendUSD > budgetUSD,
	}
	if budgetUSD <= 0 {
		bucket.RemainingBudgetUSD = 0
		return bucket
	}
	bucket.UtilizationPercent = math.Round((spendUSD/budgetUSD)*10000) / 100
	return bucket
}

func billingAlertFromBucket(scopeType string, bucket serverteam.BillingBucket) (serverteam.BudgetAlert, bool) {
	if bucket.MonthlyBudgetUSD <= 0 {
		return serverteam.BudgetAlert{}, false
	}
	severity := ""
	switch {
	case bucket.OverBudget || bucket.UtilizationPercent >= 100:
		severity = "critical"
	case bucket.UtilizationPercent >= 90:
		severity = "high"
	case bucket.UtilizationPercent >= 80:
		severity = "warning"
	default:
		return serverteam.BudgetAlert{}, false
	}
	return serverteam.BudgetAlert{
		ScopeType:          scopeType,
		ID:                 bucket.ID,
		Name:               bucket.Name,
		Severity:           severity,
		MonthlyBudgetUSD:   bucket.MonthlyBudgetUSD,
		SpendUSD:           bucket.SpendUSD,
		RemainingBudgetUSD: bucket.RemainingBudgetUSD,
		UtilizationPercent: bucket.UtilizationPercent,
		RequestCount:       bucket.RequestCount,
	}, true
}
