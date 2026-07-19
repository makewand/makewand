package serveradmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
)

// buildTenantScopedHandler wires an admin handler whose auth config contains a
// root admin, a user-scoped admin for alice, and a client token owned by bob.
func buildTenantScopedHandler(t *testing.T) (http.Handler, *router.User, *router.User, string) {
	t.Helper()
	userStore := router.NewUserStore(filepath.Join(t.TempDir(), "users"))
	alice, err := userStore.CreateUser("alice@example.com", "password123")
	if err != nil {
		t.Fatalf("CreateUser(alice): %v", err)
	}
	bob, err := userStore.CreateUser("bob@example.com", "password123")
	if err != nil {
		t.Fatalf("CreateUser(bob): %v", err)
	}

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{ID: "root", Token: "root-secret", Scopes: serverauth.AllScopes()},
			{ID: "alice-admin", Token: "alice-secret", UserID: alice.ID, Scopes: serverauth.AllScopes()},
			{ID: "bob-token", Token: "bob-secret", UserID: bob.ID, Scopes: serverauth.AllClientScopes()},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	logger, err := serveraudit.OpenJSONL(auditPath)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	logger.Log(serveraudit.Event{Timestamp: time.Now().UTC(), Kind: "chat", TokenID: "alice-admin", Status: 200})
	logger.Log(serveraudit.Event{Timestamp: time.Now().UTC(), Kind: "chat", TokenID: "bob-token", Status: 200})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		UserStore:    userStore,
		AuditPath:    auditPath,
	})
	return handler, alice, bob, "alice-secret"
}

func TestTenantScope_UserModifyRestrictedToOwnAccount(t *testing.T) {
	handler, alice, bob, aliceToken := buildTenantScopedHandler(t)

	// alice-scoped admin may not deactivate bob.
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+bob.ID+"/deactivate", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("deactivate(bob) status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// alice-scoped admin may modify her own account.
	reqSelf := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+alice.ID+"/deactivate", nil)
	reqSelf.Header.Set("Authorization", "Bearer "+aliceToken)
	recSelf := httptest.NewRecorder()
	handler.ServeHTTP(recSelf, reqSelf)
	if recSelf.Code != http.StatusOK {
		t.Fatalf("deactivate(self) status = %d, want 200; body=%s", recSelf.Code, recSelf.Body.String())
	}
}

func TestTenantScope_RevokeRestrictedToTenant(t *testing.T) {
	handler, _, _, aliceToken := buildTenantScopedHandler(t)

	// alice-scoped admin may not revoke bob's token.
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens/bob-token/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoke(bob-token) status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// alice-scoped admin may revoke her own token.
	reqSelf := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens/alice-admin/revoke", nil)
	reqSelf.Header.Set("Authorization", "Bearer "+aliceToken)
	recSelf := httptest.NewRecorder()
	handler.ServeHTTP(recSelf, reqSelf)
	if recSelf.Code != http.StatusOK {
		t.Fatalf("revoke(alice-admin) status = %d, want 200; body=%s", recSelf.Code, recSelf.Body.String())
	}
}

func TestTenantScope_DashboardFiltersUsersAndTokens(t *testing.T) {
	handler, alice, _, aliceToken := buildTenantScopedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users struct {
			Data []router.UserView `json:"data"`
		} `json:"users"`
		Tokens struct {
			Data []serverauth.TokenRuleView `json:"data"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	if len(resp.Users.Data) != 1 || resp.Users.Data[0].ID != alice.ID {
		t.Fatalf("dashboard users = %+v, want only alice", resp.Users.Data)
	}
	for _, tok := range resp.Tokens.Data {
		if tok.UserID != alice.ID {
			t.Fatalf("dashboard leaked token outside tenant: %+v", tok)
		}
	}
}

func TestTenantScope_AuditFilteredToTenant(t *testing.T) {
	handler, _, _, aliceToken := buildTenantScopedHandler(t)

	// Explicitly requesting another tenant's token is rejected.
	reqDenied := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/events?token_id=bob-token", nil)
	reqDenied.Header.Set("Authorization", "Bearer "+aliceToken)
	recDenied := httptest.NewRecorder()
	handler.ServeHTTP(recDenied, reqDenied)
	if recDenied.Code != http.StatusForbidden {
		t.Fatalf("audit token_id=bob-token status = %d, want 403; body=%s", recDenied.Code, recDenied.Body.String())
	}

	// An unfiltered query returns only in-tenant events.
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/events", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit events status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []serveraudit.Event `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode audit events: %v", err)
	}
	for _, event := range resp.Data {
		if event.TokenID != "alice-admin" {
			t.Fatalf("audit leaked event outside tenant: %+v", event)
		}
	}
}
