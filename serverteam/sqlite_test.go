package serverteam

import (
	"path/filepath"
	"testing"
)

func TestSQLiteStore_CreateAndListOrganizationsAndProjects(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	org, err := store.CreateOrganization(Organization{
		Name:             "Platform Team",
		Slug:             "platform-team",
		Description:      "Shared platform services",
		MonthlyBudgetUSD: 100,
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if org.ID == "" {
		t.Fatal("organization id = empty")
	}

	project, err := store.CreateProject(Project{
		OrganizationID:   org.ID,
		Name:             "Checkout API",
		Slug:             "checkout-api",
		Description:      "Critical payment path",
		MonthlyBudgetUSD: 25,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if project.ID == "" {
		t.Fatal("project id = empty")
	}

	orgs, err := store.ListOrganizations()
	if err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	if len(orgs) != 1 || orgs[0].ID != org.ID {
		t.Fatalf("organizations = %+v, want created organization", orgs)
	}

	projects, err := store.ListProjects(org.ID)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("projects = %+v, want created project", projects)
	}

	gotOrg, err := store.GetOrganization(org.ID)
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	if gotOrg.Name != "Platform Team" {
		t.Fatalf("organization name = %q, want Platform Team", gotOrg.Name)
	}

	gotProject, err := store.GetProject(project.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if gotProject.OrganizationID != org.ID {
		t.Fatalf("project organization_id = %q, want %q", gotProject.OrganizationID, org.ID)
	}
}

func TestSQLiteStore_CreateProjectRejectsMissingOrganization(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	_, err = store.CreateProject(Project{
		OrganizationID: "org_missing",
		Name:           "Checkout API",
	})
	if err == nil {
		t.Fatal("CreateProject() error = nil, want missing organization error")
	}
}

func TestSQLiteStore_OrganizationAndProjectMemberships(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	org, err := store.CreateOrganization(Organization{Name: "Platform Team"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	project, err := store.CreateProject(Project{
		OrganizationID: org.ID,
		Name:           "Checkout API",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	orgMembership, err := store.UpsertOrganizationMembership(OrganizationMembership{
		OrganizationID: org.ID,
		UserID:         "usr_a",
		Role:           MembershipRoleManager,
	})
	if err != nil {
		t.Fatalf("UpsertOrganizationMembership: %v", err)
	}
	if orgMembership.Role != MembershipRoleManager {
		t.Fatalf("organization membership role = %q, want %q", orgMembership.Role, MembershipRoleManager)
	}

	projectMembership, err := store.UpsertProjectMembership(ProjectMembership{
		ProjectID: project.ID,
		UserID:    "usr_a",
		Role:      MembershipRoleViewer,
	})
	if err != nil {
		t.Fatalf("UpsertProjectMembership: %v", err)
	}
	if projectMembership.OrganizationID != org.ID {
		t.Fatalf("project membership organization_id = %q, want %q", projectMembership.OrganizationID, org.ID)
	}

	orgMemberships, err := store.ListOrganizationMemberships(org.ID, "")
	if err != nil {
		t.Fatalf("ListOrganizationMemberships: %v", err)
	}
	if len(orgMemberships) != 1 || orgMemberships[0].UserID != "usr_a" {
		t.Fatalf("organization memberships = %+v, want usr_a", orgMemberships)
	}

	projectMemberships, err := store.ListProjectMemberships(project.ID, "usr_a")
	if err != nil {
		t.Fatalf("ListProjectMemberships: %v", err)
	}
	if len(projectMemberships) != 1 || projectMemberships[0].Role != MembershipRoleViewer {
		t.Fatalf("project memberships = %+v, want viewer membership", projectMemberships)
	}
}
