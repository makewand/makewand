package router

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverteam"
)

type authorizationCountingProvider struct {
	*stubProvider
	calls int
}

func (p *authorizationCountingProvider) Chat(ctx context.Context, msgs []Message, system string, max int) (string, Usage, error) {
	p.calls++
	return p.stubProvider.Chat(ctx, msgs, system, max)
}

func TestUserAuthorization_StoredCredentialInvalidationBeforeProviderCall(t *testing.T) {
	for _, change := range []string{"organization-disable", "organization-downgrade", "project-disable", "project-downgrade", "password", "deactivate-reactivate", "global-role"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			users, err := OpenSQLiteUserStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer users.Close()
			role := UserRoleMember
			if change == "global-role" {
				role = UserRoleAdmin
			}
			user, err := users.CreateUserWithRole("user@example.com", "password123", role)
			if err != nil {
				t.Fatal(err)
			}
			teams, err := serverteam.OpenSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer teams.Close()
			org, err := teams.CreateOrganization(serverteam.Organization{Name: "team"})
			if err != nil {
				t.Fatal(err)
			}
			project, err := teams.CreateProject(serverteam.Project{OrganizationID: org.ID, Name: "app"})
			if err != nil {
				t.Fatal(err)
			}
			orgMember := serverteam.OrganizationMembership{OrganizationID: org.ID, UserID: user.ID, Role: "manager", IsActive: true}
			projectMember := serverteam.ProjectMembership{ProjectID: project.ID, UserID: user.ID, Role: "manager", IsActive: true}
			if _, err := teams.UpsertOrganizationMembership(orgMember); err != nil {
				t.Fatal(err)
			}
			if _, err := teams.UpsertProjectMembership(projectMember); err != nil {
				t.Fatal(err)
			}
			tokens, err := serverauth.OpenSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tokens.Close() }()
			provider := &authorizationCountingProvider{stubProvider: &stubProvider{name: "claude", available: true, response: "ok"}}
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: provider, Access: AccessSubscription}}, DefaultModel: "claude", CodingModel: "claude"})
			handler := r.HTTPHandlerWithUsers(users, UserEndpointOptions{}, HTTPHandlerOptions{Authorizer: tokens, UserTokenManager: tokens, TeamStore: teams})
			//nolint:gosec // G117: fixed test-only login password sent to an in-memory handler, never a real credential or server.
			body, _ := json.Marshal(UserLoginRequest{Email: user.Email, Password: "password123", OrganizationID: org.ID, ProjectID: project.ID})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/users/login", bytes.NewReader(body)))
			if rec.Code != 200 {
				t.Fatalf("login=%d %s", rec.Code, rec.Body.String())
			}
			var login UserLoginResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
				t.Fatal(err)
			}
			request := func(path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"model":"claude","messages":[{"role":"user","content":"hello"}]}`))
				req.Header.Set("Authorization", "Bearer "+login.Token)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				return rec
			}
			rec = request("/v1/chat/completions")
			if rec.Code != 200 || provider.calls != 1 {
				t.Fatalf("baseline=%d calls=%d %s", rec.Code, provider.calls, rec.Body.String())
			}
			// Reload the token store before mutation to prove the authorization
			// binding is durable, rather than held only by the issuing process.
			if err := tokens.Close(); err != nil {
				t.Fatal(err)
			}
			tokens, err = serverauth.OpenSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			handler = r.HTTPHandlerWithUsers(users, UserEndpointOptions{}, HTTPHandlerOptions{Authorizer: tokens, UserTokenManager: tokens, TeamStore: teams})
			switch change {
			case "organization-disable":
				orgMember.IsActive = false
				_, err = teams.UpsertOrganizationMembership(orgMember)
			case "organization-downgrade":
				orgMember.Role = "viewer"
				_, err = teams.UpsertOrganizationMembership(orgMember)
			case "project-disable":
				projectMember.IsActive = false
				_, err = teams.UpsertProjectMembership(projectMember)
			case "project-downgrade":
				projectMember.Role = "viewer"
				_, err = teams.UpsertProjectMembership(projectMember)
			case "password":
				_, err = users.SetUserPassword(user.ID, "changed123")
			case "deactivate-reactivate":
				_, err = users.SetUserActive(user.ID, false)
				if err != nil {
					t.Fatal(err)
				}
				_, err = users.SetUserActive(user.ID, true)
			case "global-role":
				_, err = users.SetUserRole(user.ID, UserRoleMember)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/models"} {
				rec = request(path)
				if rec.Code != 401 {
					t.Fatalf("old credential %s=%d %s", path, rec.Code, rec.Body.String())
				}
			}
			if provider.calls != 1 {
				t.Fatalf("revoked credentials reached provider %d times", provider.calls-1)
			}
			// Restoring memberships does not resurrect previously issued tokens.
			orgMember.Role = "manager"
			orgMember.IsActive = true
			projectMember.Role = "manager"
			projectMember.IsActive = true
			if _, err := teams.UpsertOrganizationMembership(orgMember); err != nil {
				t.Fatal(err)
			}
			if _, err := teams.UpsertProjectMembership(projectMember); err != nil {
				t.Fatal(err)
			}
			if rec := request("/v1/chat/completions"); rec.Code != 401 {
				t.Fatalf("old credential resurrected: %d", rec.Code)
			}
		})
	}
}
