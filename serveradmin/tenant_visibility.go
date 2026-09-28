package serveradmin

import (
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
)

// tenantView is the set of organizations and projects an admin grant may see.
// It applies the same intersection as the user directory: a grant bound to a
// user only sees tenants that user actively belongs to, further narrowed by the
// grant's organization/project restriction. Missing or unreadable membership
// data never expands visibility.
type tenantView struct {
	// all is true for unrestricted (global) grants.
	all bool
	// orgs are organizations whose full records, including budgets, are visible.
	orgs map[string]bool
	// orgNames are parent organizations of visible projects that are shown
	// without organization-level budget data.
	orgNames map[string]bool
	// projects are individually visible projects.
	projects map[string]bool
	// projectOrgs are organizations whose every project is visible.
	projectOrgs map[string]bool
}

func resolveTenantView(grant *serverauth.Grant, teams serverteam.Store) tenantView {
	view := tenantView{
		orgs:        map[string]bool{},
		orgNames:    map[string]bool{},
		projects:    map[string]bool{},
		projectOrgs: map[string]bool{},
	}
	if grant == nil {
		return view
	}
	userID, orgID, projectID := grant.UserID(), grant.OrganizationID(), grant.ProjectID()
	if userID == "" && orgID == "" && projectID == "" {
		view.all = true
		return view
	}
	if teams == nil {
		return view
	}

	// For user-bound grants, derive the tenants the user actively belongs to.
	// nil maps mean "not restricted by membership" (service tokens).
	var memberOrgs map[string]bool
	var memberProjects map[string]string // project ID -> parent organization ID
	if userID != "" {
		orgMemberships, err := teams.ListOrganizationMemberships("", userID)
		if err != nil {
			return view
		}
		memberOrgs = map[string]bool{}
		disabledOrgs := map[string]bool{}
		for _, membership := range orgMemberships {
			if membership.IsActive && serverteam.RoleRank(membership.Role) > 0 {
				memberOrgs[membership.OrganizationID] = true
			} else {
				disabledOrgs[membership.OrganizationID] = true
			}
		}
		projectMemberships, err := teams.ListProjectMemberships("", userID)
		if err != nil {
			return view
		}
		memberProjects = map[string]string{}
		for _, membership := range projectMemberships {
			// An explicitly disabled parent membership blocks project access.
			if membership.IsActive && serverteam.RoleRank(membership.Role) > 0 && !disabledOrgs[membership.OrganizationID] {
				memberProjects[membership.ProjectID] = membership.OrganizationID
			}
		}
	}

	switch {
	case projectID != "":
		project, err := teams.GetProject(projectID)
		if err != nil || project == nil || (orgID != "" && project.OrganizationID != orgID) {
			return view
		}
		if memberProjects != nil {
			if _, ok := memberProjects[projectID]; !ok && !memberOrgs[project.OrganizationID] {
				return view
			}
		}
		view.projects[projectID] = true
		view.orgNames[project.OrganizationID] = true
	case orgID != "":
		if memberOrgs == nil || memberOrgs[orgID] {
			view.orgs[orgID] = true
			view.projectOrgs[orgID] = true
		}
		for id, parent := range memberProjects {
			if parent == orgID {
				view.projects[id] = true
				view.orgNames[parent] = true
			}
		}
	default:
		for id := range memberOrgs {
			view.orgs[id] = true
			view.projectOrgs[id] = true
		}
		for id, parent := range memberProjects {
			view.projects[id] = true
			view.orgNames[parent] = true
		}
	}
	return view
}

// organization returns the visible form of org: full records for visible
// organizations, a budget-redacted record for parents of visible projects.
func (v tenantView) organization(org serverteam.Organization) (serverteam.Organization, bool) {
	if v.all || v.orgs[org.ID] {
		return org, true
	}
	if v.orgNames[org.ID] {
		org.MonthlyBudgetUSD = 0
		return org, true
	}
	return org, false
}

// organizationBudgetVisible reports whether organization-level billing (budget
// and spend buckets) may be shown for id.
func (v tenantView) organizationBudgetVisible(id string) bool {
	return v.all || v.orgs[id]
}

func (v tenantView) project(project serverteam.Project) bool {
	return v.all || v.projects[project.ID] || v.projectOrgs[project.OrganizationID]
}
