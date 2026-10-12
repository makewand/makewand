package serverteam

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func openConcurrentTeamStore(t *testing.T) (*SQLiteStore, *Organization) {
	t.Helper()
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	org, err := store.CreateOrganization(Organization{Name: "Platform"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return store, org
}

func TestSQLiteStore_ConcurrentCreateProjectSameSlugExactlyOnce(t *testing.T) {
	store, org := openConcurrentTeamStore(t)
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.CreateProject(Project{ID: fmt.Sprintf("prj_%d", i), OrganizationID: org.ID, Name: "Checkout", Slug: "checkout"})
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successful creates = %d, want exactly 1", successes)
	}
	projects, err := store.ListProjects(org.ID)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(projects))
	}
}

func TestSQLiteStore_ConcurrentMembershipUpsertsNoLostOrDuplicateRows(t *testing.T) {
	store, org := openConcurrentTeamStore(t)
	project, err := store.CreateProject(Project{OrganizationID: org.ID, Name: "Checkout"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	other, err := store.CreateOrganization(Organization{Name: "Other"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	const users, repeats = 12, 4
	var wg sync.WaitGroup
	errs := make(chan error, users*repeats*3)
	for u := 0; u < users; u++ {
		for r := 0; r < repeats; r++ {
			wg.Add(1)
			go func(u int) {
				defer wg.Done()
				uid := fmt.Sprintf("usr_%d", u)
				if _, err := store.UpsertOrganizationMembership(OrganizationMembership{OrganizationID: org.ID, UserID: uid, Role: MembershipRoleMember}); err != nil {
					errs <- err
				}
				if _, err := store.UpsertProjectMembership(ProjectMembership{ProjectID: project.ID, UserID: uid, Role: MembershipRoleMember}); err != nil {
					errs <- err
				}
				// Mismatched organization must be rejected without writing.
				if _, err := store.UpsertProjectMembership(ProjectMembership{ProjectID: project.ID, OrganizationID: other.ID, UserID: uid + "_x", Role: MembershipRoleMember}); err == nil {
					errs <- fmt.Errorf("mismatched organization accepted for %s", uid)
				}
			}(u)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	orgMembers, err := store.ListOrganizationMemberships(org.ID, "")
	if err != nil {
		t.Fatalf("ListOrganizationMemberships: %v", err)
	}
	if len(orgMembers) != users {
		t.Fatalf("org memberships = %d, want %d", len(orgMembers), users)
	}
	projectMembers, err := store.ListProjectMemberships(project.ID, "")
	if err != nil {
		t.Fatalf("ListProjectMemberships: %v", err)
	}
	if len(projectMembers) != users {
		t.Fatalf("project memberships = %d, want %d", len(projectMembers), users)
	}
	for _, m := range projectMembers {
		if m.OrganizationID != org.ID {
			t.Fatalf("membership %+v has organization %q, want %q", m, m.OrganizationID, org.ID)
		}
	}
}

func TestSQLiteStore_MembershipRejectsMissingParents(t *testing.T) {
	store, _ := openConcurrentTeamStore(t)
	if _, err := store.UpsertOrganizationMembership(OrganizationMembership{OrganizationID: "org_missing", UserID: "usr", Role: MembershipRoleMember}); err == nil {
		t.Fatal("membership in missing organization accepted")
	}
	if _, err := store.UpsertProjectMembership(ProjectMembership{ProjectID: "prj_missing", UserID: "usr", Role: MembershipRoleMember}); err == nil {
		t.Fatal("membership in missing project accepted")
	}
}
