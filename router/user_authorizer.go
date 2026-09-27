package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
)

type userStateAuthorizer struct {
	base  serverauth.RequestAuthorizer
	users UserManager
	teams serverteam.Store
}

// WithUserAuthorization checks current account and tenant membership state on
// every authenticated request, including sessions, metrics, and model calls.
// Service tokens without a user remain governed by their explicit token policy.
func WithUserAuthorization(base serverauth.RequestAuthorizer, users UserManager, teams serverteam.Store) serverauth.RequestAuthorizer {
	if base == nil || (users == nil && teams == nil) {
		return base
	}
	return &userStateAuthorizer{base: base, users: users, teams: teams}
}

func (a *userStateAuthorizer) AuthenticateRequest(req *http.Request) (*serverauth.Grant, bool) {
	grant, ok := a.base.AuthenticateRequest(req)
	if !ok || grant == nil || !UserGrantIsCurrent(grant, a.users, a.teams) {
		return nil, false
	}
	return grant, true
}

// UserGrantIsCurrent fails closed when a user or assigned membership no longer
// exists or is inactive. Project membership may exist without an organization
// membership, but an explicitly disabled parent membership always takes effect.
func UserGrantIsCurrent(grant *serverauth.Grant, users UserManager, teams serverteam.Store) bool {
	if grant == nil {
		return false
	}
	userID := grant.UserID()
	if userID == "" {
		return true
	}
	if users != nil {
		user, err := users.GetUserByID(userID)
		if err != nil || user == nil || !user.IsActive {
			return false
		}
		if expected := grant.AuthorizationVersion(); expected != "" {
			current, err := UserAuthorizationVersion(user, grant.OrganizationID(), grant.ProjectID(), teams)
			if err != nil || current != expected {
				return false
			}
		}
	}
	if teams == nil || (grant.OrganizationID() == "" && grant.ProjectID() == "") {
		return true
	}
	orgID := grant.OrganizationID()
	if grant.ProjectID() != "" {
		project, err := teams.GetProject(grant.ProjectID())
		if err != nil || project == nil || !project.IsActive || (orgID != "" && project.OrganizationID != orgID) {
			return false
		}
		orgID = project.OrganizationID
		membership, err := teams.GetProjectMembership(project.ID, userID)
		if err != nil || membership == nil || !membership.IsActive || serverteam.RoleRank(membership.Role) == 0 {
			return false
		}
	}
	if orgID != "" {
		org, err := teams.GetOrganization(orgID)
		if err != nil || org == nil || !org.IsActive {
			return false
		}
		// Listing distinguishes an absent optional parent membership from a
		// failed storage read, which must not grant access.
		memberships, err := teams.ListOrganizationMemberships(orgID, userID)
		if err != nil || (len(memberships) == 0 && grant.ProjectID() == "") {
			return false
		}
		for _, membership := range memberships {
			if !membership.IsActive || serverteam.RoleRank(membership.Role) == 0 {
				return false
			}
		}
	}
	return true
}

// UserAuthorizationVersion is an opaque digest of persisted authorization state.
// Comparing it at request time invalidates stale/concurrently issued credentials
// after password resets, membership role changes, or disable/re-enable cycles.
func UserAuthorizationVersion(user *User, orgID, projectID string, teams serverteam.Store) (string, error) {
	if user == nil {
		return "", fmt.Errorf("user is unavailable")
	}
	state := []any{user.ID, user.PasswordHash, user.Salt, user.Role, user.IsActive, user.UpdatedAt}
	if teams != nil {
		if projectID != "" {
			project, err := teams.GetProject(projectID)
			if err != nil || project == nil || (orgID != "" && project.OrganizationID != orgID) {
				return "", fmt.Errorf("token project or parent organization is unavailable")
			}
			orgID = project.OrganizationID
			memberships, err := teams.ListProjectMemberships(projectID, user.ID)
			if err != nil {
				return "", err
			}
			state = append(state, memberships)
		}
		if orgID != "" {
			memberships, err := teams.ListOrganizationMemberships(orgID, user.ID)
			if err != nil {
				return "", err
			}
			state = append(state, memberships)
		}
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
