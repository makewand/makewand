package serveradmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
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

	handler := NewHandler(HandlerOptions{
		Authorizer:   manager,
		TokenManager: manager,
		AuditPath:    auditPath,
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
		Path  string         `json:"path"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.NewDecoder(usageRec.Body).Decode(&usageResp); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	if usageResp.Usage["total_cost_usd"] != 0.25 {
		t.Fatalf("usage total cost = %#v, want 0.25", usageResp.Usage["total_cost_usd"])
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
