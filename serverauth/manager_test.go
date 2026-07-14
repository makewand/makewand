package serverauth

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestManager_IssueAndRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_auth.json")
	if err := SaveConfigFile(path, Config{
		Tokens: []TokenRule{
			{
				ID:     "admin",
				Token:  "admin-secret",
				Scopes: AllScopes(),
			},
		},
	}); err != nil {
		t.Fatalf("SaveConfigFile: %v", err)
	}

	manager, err := LoadManager(path)
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}

	view, tokenValue, err := manager.Issue(TokenRule{
		ID:     "runner",
		Scopes: AllClientScopes(),
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if view.ID != "runner" {
		t.Fatalf("view.ID = %q, want %q", view.ID, "runner")
	}
	if tokenValue == "" {
		t.Fatal("tokenValue = empty")
	}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tokenValue)
	if _, ok := manager.AuthenticateRequest(req); !ok {
		t.Fatal("AuthenticateRequest(issued token) = false, want true")
	}

	if err := manager.Revoke("runner"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := manager.AuthenticateRequest(req); ok {
		t.Fatal("AuthenticateRequest(revoked token) = true, want false")
	}
}
