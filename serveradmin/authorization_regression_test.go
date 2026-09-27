package serveradmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
)

type authorizationFixture struct {
	users                    *router.SQLiteUserStore
	teams                    *serverteam.SQLiteStore
	tokens                   *serverauth.SQLiteStore
	handler                  http.Handler
	alice, bob, carol, admin *router.User
	root                     string
}

func newAuthorizationFixture(t *testing.T) *authorizationFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	users, err := router.OpenSQLiteUserStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = users.Close() })
	teams, err := serverteam.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = teams.Close() })
	tokens, err := serverauth.OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tokens.Close() })
	f := &authorizationFixture{users: users, teams: teams, tokens: tokens}
	for _, entry := range []struct {
		email, role string
		out         **router.User
	}{
		{"alice@example.com", router.UserRoleMember, &f.alice},
		{"bob@example.com", router.UserRoleMember, &f.bob},
		{"carol@example.com", router.UserRoleMember, &f.carol},
		{"admin@example.com", router.UserRoleAdmin, &f.admin},
	} {
		*entry.out, err = users.CreateUserWithRole(entry.email, "password123", entry.role)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"org-a", "org-b"} {
		if _, err := teams.CreateOrganization(serverteam.Organization{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		project, org string
		user         *router.User
	}{
		{"project-a", "org-a", f.alice}, {"project-b", "org-b", f.bob}, {"project-c", "org-a", f.carol},
	} {
		if _, err := teams.CreateProject(serverteam.Project{ID: entry.project, OrganizationID: entry.org, Name: entry.project}); err != nil {
			t.Fatal(err)
		}
		if _, err := teams.UpsertOrganizationMembership(serverteam.OrganizationMembership{OrganizationID: entry.org, UserID: entry.user.ID, Role: "manager", IsActive: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := teams.UpsertProjectMembership(serverteam.ProjectMembership{ProjectID: entry.project, UserID: entry.user.ID, Role: "manager", IsActive: true}); err != nil {
			t.Fatal(err)
		}
	}
	f.root = f.issue(t, serverauth.TokenRule{Scopes: serverauth.AllScopes()})
	f.handler = NewHandler(HandlerOptions{Authorizer: tokens, TokenManager: tokens, UserStore: users, TeamStore: teams})
	return f
}

func (f *authorizationFixture) issue(t *testing.T, rule serverauth.TokenRule) string {
	t.Helper()
	_, token, err := f.tokens.Issue(rule)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func adminRequest(handler http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthorization_UserDirectoryHonorsEveryScopeIntersection(t *testing.T) {
	f := newAuthorizationFixture(t)
	cases := []struct {
		name, user, org, project string
		want                     []string
	}{
		{name: "organization", org: "org-a", want: []string{f.alice.ID, f.carol.ID}},
		{name: "project", project: "project-a", want: []string{f.alice.ID}},
		{name: "organization-project", org: "org-a", project: "project-a", want: []string{f.alice.ID}},
		{name: "user", user: f.alice.ID, want: []string{f.alice.ID}},
		{name: "user-organization", user: f.alice.ID, org: "org-a", want: []string{f.alice.ID}},
		{name: "user-project", user: f.alice.ID, project: "project-a", want: []string{f.alice.ID}},
		{name: "user-organization-project", user: f.alice.ID, org: "org-a", project: "project-a", want: []string{f.alice.ID}},
		{name: "inconsistent-organization-project", org: "org-b", project: "project-a", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := f.issue(t, serverauth.TokenRule{UserID: tc.user, OrganizationID: tc.org, ProjectID: tc.project, Scopes: serverauth.AllScopes()})
			for _, path := range []string{"/v1/admin/users?user_id=" + f.bob.ID + "&organization_id=org-b", "/v1/admin/dashboard"} {
				rec := adminRequest(f.handler, token, http.MethodGet, path, "")
				if rec.Code != 200 {
					t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
				}
				var body struct {
					Data  []router.UserView `json:"data"`
					Users struct {
						Data []router.UserView `json:"data"`
					} `json:"users"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				got := body.Data
				if path == "/v1/admin/dashboard" {
					got = body.Users.Data
				}
				expected := map[string]bool{}
				for _, id := range tc.want {
					expected[id] = true
				}
				if len(got) != len(expected) {
					t.Fatalf("%s users=%+v; want IDs %v", path, got, tc.want)
				}
				for _, user := range got {
					if !expected[user.ID] {
						t.Fatalf("leaked user %+v", user)
					}
				}
			}
		})
	}
}

func TestAuthorization_ScopedUsersCapabilityCannotCreateGlobalAdminSession(t *testing.T) {
	f := newAuthorizationFixture(t)
	sessionMgr, err := NewSessionManager(f.users, []byte("0123456789abcdef0123456789abcdef"), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []serverauth.TokenRule{
		{Scopes: []string{serverauth.ScopeAdminUsersWrite}},
		{OrganizationID: "org-a", Scopes: serverauth.AllScopes()},
		{ProjectID: "project-a", Scopes: serverauth.AllScopes()},
		{UserID: f.alice.ID, Scopes: serverauth.AllScopes()},
		{AllowedProviders: []string{"claude"}, Scopes: serverauth.AllScopes()},
	} {
		token := f.issue(t, rule)
		rec := adminRequest(f.handler, token, http.MethodPost, "/v1/admin/users/"+f.alice.ID+"/role", `{"role":"admin"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("rule=%+v role status=%d %s", rule, rec.Code, rec.Body.String())
		}
		rec = adminRequest(f.handler, token, http.MethodPost, "/v1/admin/users/"+f.admin.ID+"/password", `{"password":"attacker123"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("rule=%+v admin-password status=%d %s", rule, rec.Code, rec.Body.String())
		}
	}
	tenantToken := f.issue(t, serverauth.TokenRule{OrganizationID: "org-a", Scopes: serverauth.AllScopes()})
	for _, target := range []*router.User{f.alice, f.bob} {
		for action, body := range map[string]string{"password": `{"password":"attacker123"}`, "activate": "", "deactivate": ""} {
			rec := adminRequest(f.handler, tenantToken, http.MethodPost, "/v1/admin/users/"+target.ID+"/"+action, body)
			if rec.Code != 403 {
				t.Fatalf("tenant %s %s: %d %s", target.Email, action, rec.Code, rec.Body.String())
			}
		}
	}
	rec := httptest.NewRecorder()
	sessionMgr.HandleSessionLogin(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/session/login", bytes.NewBufferString(`{"email":"alice@example.com","password":"password123"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member browser login=%d %s", rec.Code, rec.Body.String())
	}
	// A fully authorized global administrator can still grant the role.
	rec = adminRequest(f.handler, f.root, http.MethodPost, "/v1/admin/users/"+f.alice.ID+"/role", `{"role":"admin"}`)
	if rec.Code != 200 {
		t.Fatalf("root role change=%d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthorization_PasswordResetInvalidatesCookieAcrossSessionManagerRestart(t *testing.T) {
	f := newAuthorizationFixture(t)
	secret := []byte("0123456789abcdef0123456789abcdef")
	sessions, err := NewSessionManager(f.users, secret, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cookie, err := sessions.createSession(f.admin)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/admin/session/me", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: cookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	if _, _, ok := sessions.Authenticate(request); !ok {
		t.Fatal("fresh cookie rejected")
	}
	rec := adminRequest(f.handler, f.root, http.MethodPost, "/v1/admin/users/"+f.admin.ID+"/password", `{"password":"changed123"}`)
	if rec.Code != 200 {
		t.Fatalf("reset=%d %s", rec.Code, rec.Body.String())
	}
	sessions, err = NewSessionManager(f.users, secret, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := sessions.Authenticate(request); ok {
		t.Fatal("pre-reset cookie accepted after restart")
	}
	current, err := f.users.GetUserByID(f.admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, cookie, err = sessions.createSession(current)
	if err != nil {
		t.Fatal(err)
	}
	fresh := httptest.NewRequest(http.MethodGet, "/v1/admin/session/me", nil)
	fresh.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: cookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	if _, _, ok := sessions.Authenticate(fresh); !ok {
		t.Fatal("fresh post-reset cookie rejected")
	}
}

type failingRevocationManager struct{ serverauth.TokenManager }

func (f failingRevocationManager) Revoke(string) error { return fmt.Errorf("storage unavailable") }

func (f failingRevocationManager) RevokeByUserID(string) error {
	return fmt.Errorf("storage unavailable")
}

func TestAuthorization_RevocationFailurePreventsSecurityMutation(t *testing.T) {
	f := newAuthorizationFixture(t)
	f.issue(t, serverauth.TokenRule{UserID: f.alice.ID, OrganizationID: "org-a", Scopes: serverauth.AllClientScopes()})
	handler := NewHandler(HandlerOptions{Authorizer: f.tokens, TokenManager: failingRevocationManager{f.tokens}, UserStore: f.users, TeamStore: f.teams})
	for _, tc := range []struct{ path, body string }{
		{"/v1/admin/users/" + f.alice.ID + "/password", `{"password":"changed123"}`},
		{"/v1/admin/organization-memberships", `{"organization_id":"org-a","user_id":"` + f.alice.ID + `","role":"viewer","is_active":false}`},
	} {
		rec := adminRequest(handler, f.root, http.MethodPost, tc.path, tc.body)
		if rec.Code != 500 {
			t.Fatalf("mutation status=%d %s", rec.Code, rec.Body.String())
		}
	}
	user, err := f.users.GetUserByID(f.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !user.ValidatePassword("password123") {
		t.Fatal("password changed despite failed revocation")
	}
	membership, err := f.teams.GetOrganizationMembership("org-a", f.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !membership.IsActive || membership.Role != "manager" {
		t.Fatalf("membership changed: %+v", membership)
	}
}

func TestAuthorization_MembershipMutationsRevokeLegacyTokens(t *testing.T) {
	for _, kind := range []string{"organization", "project"} {
		for _, active := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-active-%v", kind, active), func(t *testing.T) {
				f := newAuthorizationFixture(t)
				// Legacy/config-file-style tokens have no authorization version.
				token := f.issue(t, serverauth.TokenRule{UserID: f.alice.ID, OrganizationID: "org-a", ProjectID: "project-a", Scopes: serverauth.AllClientScopes()})
				req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				if _, ok := f.tokens.AuthenticateRequest(req); !ok {
					t.Fatal("fresh token rejected")
				}
				scopeKey, scopeID := "organization_id", "org-a"
				if kind == "project" {
					scopeKey, scopeID = "project_id", "project-a"
				}
				payload, err := json.Marshal(map[string]any{scopeKey: scopeID, "user_id": f.alice.ID, "role": "viewer", "is_active": active})
				if err != nil {
					t.Fatal(err)
				}
				rec := adminRequest(f.handler, f.root, http.MethodPost, "/v1/admin/"+kind+"-memberships", string(payload))
				if rec.Code != 201 {
					t.Fatalf("membership mutation=%d %s", rec.Code, rec.Body.String())
				}
				if _, ok := f.tokens.AuthenticateRequest(req); ok {
					t.Fatal("legacy credential survived membership downgrade/deactivation")
				}
			})
		}
	}
}

func TestAuthorization_MembershipChangePreservesOtherTenantAndGlobalCredentials(t *testing.T) {
	for _, kind := range []string{"organization", "project"} {
		t.Run(kind, func(t *testing.T) {
			f := newAuthorizationFixture(t)
			if _, err := f.teams.UpsertOrganizationMembership(serverteam.OrganizationMembership{OrganizationID: "org-b", UserID: f.alice.ID, Role: "member", IsActive: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.teams.UpsertProjectMembership(serverteam.ProjectMembership{ProjectID: "project-b", UserID: f.alice.ID, Role: "member", IsActive: true}); err != nil {
				t.Fatal(err)
			}
			type credential struct {
				token    string
				affected bool
			}
			var credentials []credential
			for _, tc := range []struct {
				org, project string
				affected     bool
			}{
				{"org-a", "", kind == "organization"}, {"org-a", "project-a", true}, {"", "project-a", true},
				{"org-b", "", false}, {"org-b", "project-b", false}, {"", "", false},
			} {
				version, err := router.UserAuthorizationVersion(f.alice, tc.org, tc.project, f.teams)
				if err != nil {
					t.Fatal(err)
				}
				token := f.issue(t, serverauth.TokenRule{UserID: f.alice.ID, OrganizationID: tc.org, ProjectID: tc.project, Scopes: serverauth.AllClientScopes(), AuthorizationVersion: version})
				credentials = append(credentials, credential{token, tc.affected})
			}
			scopedAdmin := f.issue(t, serverauth.TokenRule{OrganizationID: "org-a", Scopes: []string{serverauth.ScopeAdminUsersWrite}})
			key, id := "organization_id", "org-a"
			if kind == "project" {
				key, id = "project_id", "project-a"
			}
			payload, _ := json.Marshal(map[string]any{key: id, "user_id": f.alice.ID, "role": "viewer", "is_active": true})
			rec := adminRequest(f.handler, scopedAdmin, http.MethodPost, "/v1/admin/"+kind+"-memberships", string(payload))
			if rec.Code != 201 {
				t.Fatalf("membership mutation=%d %s", rec.Code, rec.Body.String())
			}
			auth := router.WithUserAuthorization(f.tokens, f.users, f.teams)
			for _, credential := range credentials {
				req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				req.Header.Set("Authorization", "Bearer "+credential.token)
				_, ok := auth.AuthenticateRequest(req)
				if ok == credential.affected {
					t.Fatalf("credential affected=%v authenticated=%v", credential.affected, ok)
				}
			}
		})
	}
}

func TestAuthorization_InvalidMembershipPayloadDoesNotRevokeCredentials(t *testing.T) {
	f := newAuthorizationFixture(t)
	token := f.issue(t, serverauth.TokenRule{UserID: f.alice.ID, OrganizationID: "org-a", Scopes: serverauth.AllClientScopes()})
	for _, payload := range []string{
		`{"organization_id":"org-a","user_id":"` + f.alice.ID + `","role":"invalid"}`,
		`{"organization_id":"missing","user_id":"` + f.alice.ID + `","role":"viewer"}`,
		`{"organization_id":"org-a","user_id":"missing","role":"viewer"}`,
	} {
		rec := adminRequest(f.handler, f.root, http.MethodPost, "/v1/admin/organization-memberships", payload)
		if rec.Code != 400 {
			t.Fatalf("invalid request=%d %s", rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if _, ok := f.tokens.AuthenticateRequest(req); !ok {
		t.Fatal("invalid request revoked valid credential")
	}
}

func TestAuthorization_AmbiguousTokenIDCannotCrossTenant(t *testing.T) {
	f := newAuthorizationFixture(t)
	_, _, err := f.tokens.Issue(serverauth.TokenRule{ID: "shared-id", Token: "alice-shared", UserID: f.alice.ID, OrganizationID: "org-a", Scopes: serverauth.AllClientScopes()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := serverauth.SaveConfigFile(path, serverauth.Config{Tokens: []serverauth.TokenRule{{ID: "shared-id", Token: "bob-shared", UserID: f.bob.ID, OrganizationID: "org-b", Scopes: serverauth.AllClientScopes()}}}); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := serverauth.LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := serverauth.NewMultiTokenManager(f.tokens, bootstrap)
	token := f.issue(t, serverauth.TokenRule{OrganizationID: "org-a", Scopes: serverauth.AllScopes()})
	handler := NewHandler(HandlerOptions{Authorizer: manager, TokenManager: manager, UserStore: f.users, TeamStore: f.teams})
	rec := adminRequest(handler, token, http.MethodPost, "/v1/admin/tokens/shared-id/revoke", "")
	if rec.Code != 403 {
		t.Fatalf("ambiguous revoke=%d %s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{"alice-shared", "bob-shared"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		if _, ok := manager.AuthenticateRequest(req); !ok {
			t.Fatalf("ambiguous scoped revoke modified %s", secret)
		}
	}
}
