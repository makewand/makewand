package serveradmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
)

// go-server#1: a token bound only to a user (the self-service delegation shape
// accepted by authorizeUserAction) must not be able to add its owner to an
// unrelated organization/project or raise its own membership role.
func TestG8_UserBoundTokenCannotSelfGrantMemberships(t *testing.T) {
	f := newAuthorizationFixture(t)
	selfService := f.issue(t, serverauth.TokenRule{UserID: f.bob.ID, Scopes: []string{serverauth.ScopeAdminUsersRead, serverauth.ScopeAdminUsersWrite}})
	userOrgScoped := f.issue(t, serverauth.TokenRule{UserID: f.bob.ID, OrganizationID: "org-b", Scopes: serverauth.AllScopes()})
	userProjectScoped := f.issue(t, serverauth.TokenRule{UserID: f.bob.ID, ProjectID: "project-b", Scopes: serverauth.AllScopes()})

	cases := []struct {
		name, token, path, body string
	}{
		{"cross-tenant-org-owner", selfService, "/v1/admin/organization-memberships", `{"organization_id":"org-a","user_id":"` + f.bob.ID + `","role":"owner"}`},
		{"cross-tenant-project-owner", selfService, "/v1/admin/project-memberships", `{"project_id":"project-a","user_id":"` + f.bob.ID + `","role":"owner"}`},
		{"own-org-role-escalation", selfService, "/v1/admin/organization-memberships", `{"organization_id":"org-b","user_id":"` + f.bob.ID + `","role":"owner"}`},
		{"own-project-role-escalation", selfService, "/v1/admin/project-memberships", `{"project_id":"project-b","user_id":"` + f.bob.ID + `","role":"owner"}`},
		{"user-org-scoped-escalation", userOrgScoped, "/v1/admin/organization-memberships", `{"organization_id":"org-b","user_id":"` + f.bob.ID + `","role":"owner"}`},
		{"user-project-scoped-escalation", userProjectScoped, "/v1/admin/project-memberships", `{"project_id":"project-b","user_id":"` + f.bob.ID + `","role":"owner"}`},
		{"user-bound-create-organization", selfService, "/v1/admin/organizations", `{"id":"org-evil","name":"Evil"}`},
		{"user-bound-create-project", selfService, "/v1/admin/projects", `{"id":"project-evil","organization_id":"org-a","name":"Evil"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := adminRequest(f.handler, tc.token, http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	if m, err := f.teams.GetOrganizationMembership("org-a", f.bob.ID); err == nil && m != nil {
		t.Fatalf("bob gained org-a membership: %+v", m)
	}
	if m, err := f.teams.GetProjectMembership("project-a", f.bob.ID); err == nil && m != nil {
		t.Fatalf("bob gained project-a membership: %+v", m)
	}
	if m, err := f.teams.GetOrganizationMembership("org-b", f.bob.ID); err != nil || m == nil || m.Role != "manager" {
		t.Fatalf("bob org-b membership changed: %+v err=%v", m, err)
	}
	if org, _ := f.teams.GetOrganization("org-evil"); org != nil {
		t.Fatalf("user-bound token created an organization: %+v", org)
	}

	// Tenant administrators and global administrators keep their authority.
	tenantAdmin := f.issue(t, serverauth.TokenRule{OrganizationID: "org-b", Scopes: []string{serverauth.ScopeAdminUsersWrite}})
	rec := adminRequest(f.handler, tenantAdmin, http.MethodPost, "/v1/admin/organization-memberships", `{"organization_id":"org-b","user_id":"`+f.bob.ID+`","role":"owner"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tenant admin membership update=%d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(f.handler, tenantAdmin, http.MethodPost, "/v1/admin/organization-memberships", `{"organization_id":"org-a","user_id":"`+f.bob.ID+`","role":"owner"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenant admin cross-tenant membership=%d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(f.handler, f.root, http.MethodPost, "/v1/admin/project-memberships", `{"project_id":"project-a","user_id":"`+f.bob.ID+`","role":"viewer"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("root membership update=%d %s", rec.Code, rec.Body.String())
	}
}

func loginToken(t *testing.T, f *authorizationFixture, email, extra string) (string, int) {
	t.Helper()
	login := new(router.Router).HandleUserLogin(f.users, f.tokens, f.teams, nil)
	body := `{"email":"` + email + `","password":"password123"` + extra + `}`
	rec := httptest.NewRecorder()
	login(rec, httptest.NewRequest(http.MethodPost, "/v1/users/login", bytes.NewBufferString(body)))
	if rec.Code != http.StatusOK {
		return "", rec.Code
	}
	var resp router.UserLoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Token, rec.Code
}

// go-server#8: the token a global administrator receives from /v1/users/login
// (and therefore from `makewand user login`) must manage other users, while
// tenant-scoped login tokens and non-admin tokens keep the R01 boundaries.
func TestG8_GlobalAdminLoginTokenManagesOtherUsers(t *testing.T) {
	f := newAuthorizationFixture(t)
	token, code := loginToken(t, f, "admin@example.com", "")
	if code != http.StatusOK {
		t.Fatalf("admin login=%d", code)
	}
	rec := adminRequest(f.handler, token, http.MethodGet, "/v1/admin/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list users=%d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []router.UserView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 4 {
		t.Fatalf("global admin login token sees %d users, want 4: %+v", len(list.Data), list.Data)
	}
	rec = adminRequest(f.handler, token, http.MethodPost, "/v1/admin/users/"+f.bob.ID+"/password", `{"password":"rotated123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login token password reset=%d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(f.handler, token, http.MethodPost, "/v1/admin/users/"+f.carol.ID+"/role", `{"role":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login token role change=%d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(f.handler, token, http.MethodPost, "/v1/admin/organization-memberships", `{"organization_id":"org-b","user_id":"`+f.alice.ID+`","role":"viewer"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin login token membership=%d %s", rec.Code, rec.Body.String())
	}

	// A tenant-scoped login of the same administrator stays tenant-bound (R01).
	if _, err := f.teams.UpsertOrganizationMembership(serverteam.OrganizationMembership{OrganizationID: "org-a", UserID: f.admin.ID, Role: "owner", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	scoped, code := loginToken(t, f, "admin@example.com", `,"organization_id":"org-a"`)
	if code != http.StatusOK {
		t.Fatalf("scoped admin login=%d", code)
	}
	rec = adminRequest(f.handler, scoped, http.MethodPost, "/v1/admin/users/"+f.alice.ID+"/password", `{"password":"attacker123"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenant-scoped admin login token password reset=%d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(f.handler, scoped, http.MethodPost, "/v1/admin/organization-memberships", `{"organization_id":"org-b","user_id":"`+f.alice.ID+`","role":"owner"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenant-scoped admin login token cross-tenant membership=%d %s", rec.Code, rec.Body.String())
	}

	// Non-admin login tokens and restricted admin-user tokens are not elevated.
	member, code := loginToken(t, f, "alice@example.com", `,"organization_id":"org-a"`)
	if code != http.StatusOK {
		t.Fatalf("member login=%d", code)
	}
	if rec := adminRequest(f.handler, member, http.MethodGet, "/v1/admin/users", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member login token admin list=%d %s", rec.Code, rec.Body.String())
	}
	restricted := f.issue(t, serverauth.TokenRule{UserID: f.admin.ID, Scopes: []string{serverauth.ScopeAdminUsersRead, serverauth.ScopeAdminUsersWrite}})
	rec = adminRequest(f.handler, restricted, http.MethodPost, "/v1/admin/users/"+f.alice.ID+"/role", `{"role":"admin"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted admin-user token role change=%d %s", rec.Code, rec.Body.String())
	}

	// Demoting the administrator immediately removes the elevated capability.
	rec = adminRequest(f.handler, f.root, http.MethodPost, "/v1/admin/users/"+f.admin.ID+"/role", `{"role":"member"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote=%d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(f.handler, token, http.MethodGet, "/v1/admin/users", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("demoted admin login token still accepted: %s", rec.Body.String())
	}
}

type tenantListing struct {
	Data          []json.RawMessage `json:"data"`
	Organizations struct {
		Data []serverteam.Organization `json:"data"`
	} `json:"organizations"`
	Projects struct {
		Data []serverteam.Project `json:"data"`
	} `json:"projects"`
	Billing serverteam.BillingSummary `json:"billing"`
	Alerts  []serverteam.BudgetAlert  `json:"alerts"`
}

func decodeListing(t *testing.T, rec *httptest.ResponseRecorder) tenantListing {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	var out tenantListing
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func idsOf[T any](items []T, id func(T) string) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		out[id(item)] = true
	}
	return out
}

// go-server#9: user-bound admin read tokens only list the organizations and
// projects the user belongs to; project-scoped tokens do not see the parent
// organization's budget.
func TestG8_UserBoundReadTokenOnlyListsAuthorizedTenants(t *testing.T) {
	f := newAuthorizationFixture(t)
	// Give every tenant a budget so leaks are observable.
	if _, err := f.teams.CreateOrganization(serverteam.Organization{ID: "org-c", Name: "org-c", MonthlyBudgetUSD: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.teams.CreateProject(serverteam.Project{ID: "project-d", OrganizationID: "org-c", Name: "project-d", MonthlyBudgetUSD: 7}); err != nil {
		t.Fatal(err)
	}
	// carol is also a plain member of project-d without an org-c membership.
	if _, err := f.teams.UpsertProjectMembership(serverteam.ProjectMembership{ProjectID: "project-d", UserID: f.carol.ID, Role: "member", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	orgID := func(o serverteam.Organization) string { return o.ID }
	projectID := func(p serverteam.Project) string { return p.ID }

	bob := f.issue(t, serverauth.TokenRule{UserID: f.bob.ID, Scopes: []string{serverauth.ScopeAdminUsersRead, serverauth.ScopeAdminUsageRead}})
	orgs := decodeListing(t, adminRequest(f.handler, bob, http.MethodGet, "/v1/admin/organizations", ""))
	var orgItems []serverteam.Organization
	for _, raw := range orgs.Data {
		var o serverteam.Organization
		_ = json.Unmarshal(raw, &o)
		orgItems = append(orgItems, o)
	}
	if got := idsOf(orgItems, orgID); len(got) != 1 || !got["org-b"] {
		t.Fatalf("bob organizations=%v, want only org-b", got)
	}
	projects := decodeListing(t, adminRequest(f.handler, bob, http.MethodGet, "/v1/admin/projects", ""))
	var projectItems []serverteam.Project
	for _, raw := range projects.Data {
		var p serverteam.Project
		_ = json.Unmarshal(raw, &p)
		projectItems = append(projectItems, p)
	}
	if got := idsOf(projectItems, projectID); len(got) != 1 || !got["project-b"] {
		t.Fatalf("bob projects=%v, want only project-b", got)
	}
	dash := decodeListing(t, adminRequest(f.handler, bob, http.MethodGet, "/v1/admin/dashboard", ""))
	if got := idsOf(dash.Organizations.Data, orgID); len(got) != 1 || !got["org-b"] {
		t.Fatalf("bob dashboard orgs=%v", got)
	}
	if got := idsOf(dash.Projects.Data, projectID); len(got) != 1 || !got["project-b"] {
		t.Fatalf("bob dashboard projects=%v", got)
	}
	billing := decodeListing(t, adminRequest(f.handler, bob, http.MethodGet, "/v1/admin/billing/summary", ""))
	if got := idsOf(billing.Billing.Organizations, func(b serverteam.BillingBucket) string { return b.ID }); len(got) != 1 || !got["org-b"] {
		t.Fatalf("bob billing orgs=%v", got)
	}
	if got := idsOf(billing.Billing.Projects, func(b serverteam.BillingBucket) string { return b.ID }); len(got) != 1 || !got["project-b"] {
		t.Fatalf("bob billing projects=%v", got)
	}

	// carol belongs to org-a (all of its projects) and only to project-d in
	// org-c: she sees project-d and at most org-c's name, never its budget.
	carol := f.issue(t, serverauth.TokenRule{UserID: f.carol.ID, Scopes: []string{serverauth.ScopeAdminUsersRead, serverauth.ScopeAdminUsageRead}})
	dash = decodeListing(t, adminRequest(f.handler, carol, http.MethodGet, "/v1/admin/dashboard", ""))
	if got := idsOf(dash.Projects.Data, projectID); len(got) != 3 || !got["project-a"] || !got["project-c"] || !got["project-d"] {
		t.Fatalf("carol dashboard projects=%v", got)
	}
	billing = decodeListing(t, adminRequest(f.handler, carol, http.MethodGet, "/v1/admin/billing/summary", ""))
	if got := idsOf(billing.Billing.Organizations, func(b serverteam.BillingBucket) string { return b.ID }); len(got) != 1 || !got["org-a"] {
		t.Fatalf("carol billing orgs=%v, want only org-a", got)
	}
	for _, org := range dash.Organizations.Data {
		if org.ID == "org-c" && org.MonthlyBudgetUSD != 0 {
			t.Fatalf("project-only member saw parent org budget: %+v", org)
		}
		if org.ID == "org-b" {
			t.Fatalf("carol saw unrelated org: %+v", org)
		}
	}

	// Project-scoped service tokens no longer see the parent organization budget.
	projectToken := f.issue(t, serverauth.TokenRule{ProjectID: "project-d", Scopes: serverauth.AllScopes()})
	billing = decodeListing(t, adminRequest(f.handler, projectToken, http.MethodGet, "/v1/admin/billing/summary", ""))
	if len(billing.Billing.Organizations) != 0 {
		t.Fatalf("project token billing exposes organizations: %+v", billing.Billing.Organizations)
	}
	if got := idsOf(billing.Billing.Projects, func(b serverteam.BillingBucket) string { return b.ID }); len(got) != 1 || !got["project-d"] {
		t.Fatalf("project token billing projects=%v", got)
	}
	dash = decodeListing(t, adminRequest(f.handler, projectToken, http.MethodGet, "/v1/admin/dashboard", ""))
	for _, org := range dash.Organizations.Data {
		if org.MonthlyBudgetUSD != 0 {
			t.Fatalf("project token saw organization budget: %+v", org)
		}
	}

	// Global and organization-scoped tokens are unchanged.
	root := decodeListing(t, adminRequest(f.handler, f.root, http.MethodGet, "/v1/admin/dashboard", ""))
	if len(root.Organizations.Data) != 3 || len(root.Projects.Data) != 4 {
		t.Fatalf("root dashboard orgs=%d projects=%d", len(root.Organizations.Data), len(root.Projects.Data))
	}
	orgToken := f.issue(t, serverauth.TokenRule{OrganizationID: "org-c", Scopes: serverauth.AllScopes()})
	billing = decodeListing(t, adminRequest(f.handler, orgToken, http.MethodGet, "/v1/admin/billing/summary", ""))
	if len(billing.Billing.Organizations) != 1 || billing.Billing.Organizations[0].MonthlyBudgetUSD != 30 {
		t.Fatalf("org token billing=%+v", billing.Billing.Organizations)
	}
}
