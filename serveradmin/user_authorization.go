package serveradmin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
)

func userWithinTenant(grant *serverauth.Grant, userID string, teams serverteam.Store) bool {
	if grant == nil {
		return false
	}
	if grant.UserID() != "" && grant.UserID() != userID {
		return false
	}
	if grant.OrganizationID() == "" && grant.ProjectID() == "" {
		return true
	}
	if teams == nil {
		return false
	}
	// Reuse the same intersection and parent-membership rules as request auth.
	target, err := serverauth.GrantFromRule(serverauth.TokenRule{
		UserID: userID, OrganizationID: grant.OrganizationID(), ProjectID: grant.ProjectID(),
		Scopes: []string{serverauth.ScopeModelsRead},
	})
	return err == nil && router.UserGrantIsCurrent(target, nil, teams)
}

func isGlobalAdministrator(grant *serverauth.Grant) bool {
	return grant != nil && grant.UserID() == "" && hasUnrestrictedCapabilities(grant)
}

// hasUnrestrictedCapabilities reports whether grant carries every server scope
// without tenant, workspace, provider, mode, or quota restrictions. It ignores
// the user binding so callers can decide separately how to treat it.
func hasUnrestrictedCapabilities(grant *serverauth.Grant) bool {
	if grant == nil || grant.OrganizationID() != "" || grant.ProjectID() != "" {
		return false
	}
	if len(grant.WorkspacePrefixes()) != 0 || len(grant.AllowedProviders()) != 0 || len(grant.AllowedModes()) != 0 || grant.MaxRequestsPerHour() > 0 || grant.MaxRequestsPerDay() > 0 || grant.MaxCostUSDPerDay() > 0 || grant.MaxCostUSDPerMonth() > 0 {
		return false
	}
	// Assigning admin enables an unrestricted browser session. The issuer must
	// already hold every resulting capability, not merely users:write.
	for _, scope := range serverauth.AllScopes() {
		if !grant.AllowsScope(scope) {
			return false
		}
	}
	return true
}

// elevateGlobalAdministratorGrant gives the unrestricted, full-scope token of a
// currently active global administrator account (for example the token that
// /v1/users/login and `makewand user login` return to an admin) the same
// unbound capability set as that administrator's browser session. The account
// role and active state are read from the user store on every request, so a
// demotion or deactivation removes the elevation immediately. Tenant-scoped or
// otherwise restricted tokens, and tokens of non-admin accounts, keep their
// user binding and every R01 tenant boundary.
func elevateGlobalAdministratorGrant(grant *serverauth.Grant, users router.UserManager) *serverauth.Grant {
	if grant == nil || grant.UserID() == "" || users == nil || !hasUnrestrictedCapabilities(grant) {
		return grant
	}
	user, err := users.GetUserByID(grant.UserID())
	if err != nil || user == nil || !user.IsActive || !strings.EqualFold(user.Role, router.UserRoleAdmin) {
		return grant
	}
	elevated, err := serverauth.GrantFromRule(serverauth.TokenRule{
		ID:          grant.TokenID(),
		Description: grant.Description(),
		Scopes:      serverauth.AllScopes(),
		ExpiresAt:   grant.ExpiresAt(),
	})
	if err != nil {
		return grant
	}
	return elevated
}

// forbidUserBoundTenantAdministration rejects tenant-structure mutations
// (organizations, projects, and memberships) from tokens bound to a user.
// Such tokens exist for self-service account actions; membership and tenant
// changes require a global administrator or a tenant administrator token
// (organization- or project-scoped, not bound to a user). Without this rule a
// user-bound admin:users:write token could add its owner to any tenant as an
// owner or raise its own role.
func forbidUserBoundTenantAdministration(grant *serverauth.Grant) error {
	if grant != nil && grant.UserID() != "" {
		return fmt.Errorf("user-bound tokens cannot manage organizations, projects, or memberships; use a global or tenant administrator token")
	}
	return nil
}

func authorizeUserAction(grant *serverauth.Grant, userID, action string, opts HandlerOptions) error {
	if !userWithinTenant(grant, userID, opts.TeamStore) {
		return fmt.Errorf("target user is outside the token's authorized membership scope")
	}
	// Passwords, active state, and user roles belong to the global account and
	// affect every tenant. Tenant administrators manage membership endpoints.
	if grant.OrganizationID() != "" || grant.ProjectID() != "" {
		return fmt.Errorf("tenant-scoped tokens cannot modify global accounts; manage tenant memberships instead")
	}
	user, err := opts.UserStore.GetUserByID(userID)
	if err != nil {
		return fmt.Errorf("target user is unavailable")
	}
	if (action == "role" || user.Role == router.UserRoleAdmin) && !isGlobalAdministrator(grant) {
		return fmt.Errorf("global administrator permissions are required to change global roles or administrator accounts")
	}
	return nil
}

func revokeUserCredentials(w http.ResponseWriter, req *http.Request, opts HandlerOptions, grant *serverauth.Grant, userID, kind string) bool {
	if opts.TokenManager == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential revocation is unavailable")
		return false
	}
	if err := opts.TokenManager.RevokeByUserID(userID); err != nil {
		writeError(w, http.StatusInternalServerError, "credential_revocation_failed", "could not revoke existing user tokens")
		logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, kind, http.StatusInternalServerError, err.Error(), 0, 0, 0)
		return false
	}
	return true
}

func validateMembershipTarget(opts HandlerOptions, userID, orgID, projectID, role string) error {
	userID, orgID, projectID = strings.TrimSpace(userID), strings.TrimSpace(orgID), strings.TrimSpace(projectID)
	if userID == "" {
		return fmt.Errorf("membership user_id is required")
	}
	if opts.UserStore != nil {
		if user, err := opts.UserStore.GetUserByID(userID); err != nil || user == nil {
			return fmt.Errorf("membership user is unavailable")
		}
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role != "" && serverteam.RoleRank(role) == 0 {
		return fmt.Errorf("membership role is invalid")
	}
	switch {
	case projectID != "":
		if project, err := opts.TeamStore.GetProject(projectID); err != nil || project == nil {
			return fmt.Errorf("membership project is unavailable")
		}
	case orgID != "":
		if org, err := opts.TeamStore.GetOrganization(orgID); err != nil || org == nil {
			return fmt.Errorf("membership organization is unavailable")
		}
	default:
		return fmt.Errorf("membership organization_id or project_id is required")
	}
	return nil
}

// Tenant membership authority cannot revoke the same person's unrelated tenant
// or global credentials. Resolve the complete target set before any revocation.
func revokeMembershipCredentials(w http.ResponseWriter, req *http.Request, opts HandlerOptions, grant *serverauth.Grant, userID, orgID, projectID string) bool {
	if opts.TokenManager == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential revocation is unavailable")
		return false
	}
	views := opts.TokenManager.TokenRules()
	targets := make(map[string]bool)
	matching := make([]bool, len(views))
	for i, view := range views {
		if view.UserID != userID {
			continue
		}
		affected := projectID != "" && view.ProjectID == projectID
		if projectID == "" {
			affected = view.OrganizationID == orgID
			if !affected && view.ProjectID != "" {
				project, err := opts.TeamStore.GetProject(view.ProjectID)
				if err != nil || project == nil {
					writeError(w, http.StatusInternalServerError, "credential_revocation_failed", "could not resolve token project")
					return false
				}
				affected = project.OrganizationID == orgID
			}
		}
		matching[i] = affected
		if affected && !view.Revoked {
			targets[view.ID] = true
		}
	}
	// Multiple backing stores may contain the same legacy ID. An ID-only revoke
	// must never also revoke an out-of-scope credential with that ID.
	for i, view := range views {
		if targets[view.ID] && !matching[i] {
			writeError(w, http.StatusConflict, "ambiguous_token_id", "token id is shared with an unrelated tenant")
			return false
		}
	}
	for id := range targets {
		if err := opts.TokenManager.Revoke(id); err != nil {
			writeError(w, http.StatusInternalServerError, "credential_revocation_failed", "could not revoke existing membership tokens")
			logAdminEvent(opts.AuditLogger, req, grant, serverauth.ScopeAdminUsersWrite, "admin_memberships", http.StatusInternalServerError, err.Error(), 0, 0, 0)
			return false
		}
	}
	return true
}
