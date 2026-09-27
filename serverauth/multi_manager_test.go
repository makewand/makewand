package serverauth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestMultiTokenManager_RevokesBootstrapAndDatabaseUserCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := SaveConfigFile(path, Config{Tokens: []TokenRule{{ID: "bootstrap", Token: "bootstrap-secret", UserID: "user", Scopes: AllClientScopes()}, {ID: "service", Token: "service-secret", Scopes: AllScopes()}}}); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}
	database, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := NewMultiTokenManager(database, bootstrap)
	_, token, err := manager.Issue(TokenRule{UserID: "user", Scopes: AllClientScopes()})
	if err != nil {
		t.Fatal(err)
	}
	authenticate := func(token string) bool {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		_, ok := manager.AuthenticateRequest(req)
		return ok
	}
	if !authenticate("bootstrap-secret") || !authenticate(token) {
		t.Fatal("fresh credentials rejected")
	}
	if err := manager.RevokeByUserID("user"); err != nil {
		t.Fatal(err)
	}
	if authenticate("bootstrap-secret") || authenticate(token) {
		t.Fatal("old user credential survived revocation")
	}
	if !authenticate("service-secret") {
		t.Fatal("unrelated service credential was revoked")
	}
	if len(manager.TokenRules()) != 3 {
		t.Fatalf("combined token directory=%+v", manager.TokenRules())
	}
	reloaded, err := LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer bootstrap-secret")
	if _, ok := reloaded.AuthenticateRequest(req); ok {
		t.Fatal("bootstrap revocation did not persist")
	}
}
