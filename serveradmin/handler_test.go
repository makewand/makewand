package serveradmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

func TestHandler_TokenLifecycleAndAuditQueries(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "admin",
				Token:  "admin-secret",
				Scopes: serverauth.AllScopes(),
			},
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
	logger.Log(serveraudit.Event{
		Timestamp:        time.Date(2026, 3, 23, 0, 0, 0, 0, time.UTC),
		Kind:             "chat",
		TokenID:          "runner",
		Status:           200,
		ActualProvider:   "codex",
		PromptTokens:     10,
		CompletionTokens: 5,
		CostUSD:          0.25,
	})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	usagePath := filepath.Join(t.TempDir(), "usage.jsonl")
	usageLogger, err := serverusage.OpenJSONL(usagePath)
	if err != nil {
		t.Fatalf("OpenJSONL(usage): %v", err)
	}
	usageLogger.Log(serverusage.Entry{
		Timestamp:        time.Date(2026, 3, 23, 1, 0, 0, 0, time.UTC),
		TokenID:          "runner",
		ActualProvider:   "codex",
		Status:           200,
		PromptTokens:     12,
		CompletionTokens: 8,
		CostUSD:          0.5,
	})
	if err := usageLogger.Close(); err != nil {
		t.Fatalf("Close(usage): %v", err)
	}
	userStore := router.NewUserStore(filepath.Join(t.TempDir(), "users"))
	user, err := userStore.CreateUser("person@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		AuditPath:    auditPath,
		UsagePath:    usagePath,
		UserStore:    userStore,
	})

	createBody := []byte(`{"id":"runner","allowed_providers":["codex"],"allowed_modes":["balanced"],"workspace_prefixes":["repo-"]}`)
	createReq := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer admin-secret")
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		TokenID string                   `json:"token_id"`
		Token   string                   `json:"token"`
		Rule    serverauth.TokenRuleView `json:"rule"`
	}
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.TokenID != "runner" {
		t.Fatalf("created.TokenID = %q, want runner", created.TokenID)
	}
	if created.Token == "" {
		t.Fatal("created.Token = empty")
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/admin/tokens", nil)
	listReq.Header.Set("Authorization", "Bearer admin-secret")
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body: %s", listRec.Code, listRec.Body.String())
	}
	if bytes.Contains(listRec.Body.Bytes(), []byte(created.Token)) {
		t.Fatalf("list response leaked raw token: %s", listRec.Body.String())
	}

	summaryReq := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/summary?token_id=runner", nil)
	summaryReq.Header.Set("Authorization", "Bearer admin-secret")
	summaryRec := httptest.NewRecorder()
	handler.ServeHTTP(summaryRec, summaryReq)
	if summaryRec.Code != http.StatusOK {
		t.Fatalf("summary status = %d, want 200; body: %s", summaryRec.Code, summaryRec.Body.String())
	}
	var summaryResp struct {
		Path    string              `json:"path"`
		Summary serveraudit.Summary `json:"summary"`
	}
	if err := json.NewDecoder(summaryRec.Body).Decode(&summaryResp); err != nil {
		t.Fatalf("decode summary response: %v", err)
	}
	if summaryResp.Summary.Total != 1 {
		t.Fatalf("summary total = %d, want 1", summaryResp.Summary.Total)
	}
	if summaryResp.Summary.TotalCostUSD != 0.25 {
		t.Fatalf("summary total cost = %.2f, want 0.25", summaryResp.Summary.TotalCostUSD)
	}

	usageReq := httptest.NewRequest(http.MethodGet, "/v1/admin/usage/summary?token_id=runner", nil)
	usageReq.Header.Set("Authorization", "Bearer admin-secret")
	usageRec := httptest.NewRecorder()
	handler.ServeHTTP(usageRec, usageReq)
	if usageRec.Code != http.StatusOK {
		t.Fatalf("usage status = %d, want 200; body: %s", usageRec.Code, usageRec.Body.String())
	}
	var usageResp struct {
		Path  string              `json:"path"`
		Usage serverusage.Summary `json:"usage"`
	}
	if err := json.NewDecoder(usageRec.Body).Decode(&usageResp); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	if usageResp.Path != usagePath {
		t.Fatalf("usage path = %q, want %q", usageResp.Path, usagePath)
	}
	if usageResp.Usage.TotalCostUSD != 0.5 {
		t.Fatalf("usage total cost = %#v, want 0.5", usageResp.Usage.TotalCostUSD)
	}

	usersReq := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil)
	usersReq.Header.Set("Authorization", "Bearer admin-secret")
	usersRec := httptest.NewRecorder()
	handler.ServeHTTP(usersRec, usersReq)
	if usersRec.Code != http.StatusOK {
		t.Fatalf("users status = %d, want 200; body: %s", usersRec.Code, usersRec.Body.String())
	}
	var usersResp struct {
		Data []router.UserView `json:"data"`
	}
	if err := json.NewDecoder(usersRec.Body).Decode(&usersResp); err != nil {
		t.Fatalf("decode users response: %v", err)
	}
	if len(usersResp.Data) != 1 || usersResp.Data[0].ID != user.ID {
		t.Fatalf("users response = %+v, want created user %s", usersResp.Data, user.ID)
	}

	roleReq := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+user.ID+"/role", bytes.NewBufferString(`{"role":"admin"}`))
	roleReq.Header.Set("Authorization", "Bearer admin-secret")
	roleReq.Header.Set("Content-Type", "application/json")
	roleRec := httptest.NewRecorder()
	handler.ServeHTTP(roleRec, roleReq)
	if roleRec.Code != http.StatusOK {
		t.Fatalf("role status = %d, want 200; body: %s", roleRec.Code, roleRec.Body.String())
	}

	deactivateReq := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+user.ID+"/deactivate", nil)
	deactivateReq.Header.Set("Authorization", "Bearer admin-secret")
	deactivateRec := httptest.NewRecorder()
	handler.ServeHTTP(deactivateRec, deactivateReq)
	if deactivateRec.Code != http.StatusOK {
		t.Fatalf("deactivate status = %d, want 200; body: %s", deactivateRec.Code, deactivateRec.Body.String())
	}

	updatedUser, err := userStore.GetUserByID(user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if updatedUser.Role != router.UserRoleAdmin {
		t.Fatalf("updated role = %q, want %q", updatedUser.Role, router.UserRoleAdmin)
	}
	if updatedUser.IsActive {
		t.Fatal("updated user still active, want deactivated")
	}

	revokeReq := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens/runner/revoke", nil)
	revokeReq.Header.Set("Authorization", "Bearer admin-secret")
	revokeRec := httptest.NewRecorder()
	handler.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200; body: %s", revokeRec.Code, revokeRec.Body.String())
	}
}

func TestHandler_RequiresAdminScopes(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "viewer",
				Token:  "viewer-secret",
				Scopes: serverauth.AllClientScopes(),
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tokens", nil)
	req.Header.Set("Authorization", "Bearer viewer-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_UserEndpointsRequireUserScopes(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "server_auth.json")
	if err := serverauth.SaveConfigFile(authPath, serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "viewer",
				Token:  "viewer-secret",
				Scopes: []string{serverauth.ScopeAdminAuditRead},
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}
	manager, err := serverauth.LoadManager(authPath)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		UserStore:    router.NewUserStore(filepath.Join(t.TempDir(), "users")),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil)
	req.Header.Set("Authorization", "Bearer viewer-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}
