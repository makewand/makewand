package serverauth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewAuthorizer_ValidatesRules(t *testing.T) {
	if _, err := NewAuthorizer(Config{}); err == nil {
		t.Fatal("NewAuthorizer(Config{}) error = nil, want validation error")
	}

	_, err := NewAuthorizer(Config{
		Tokens: []TokenRule{
			{Token: "a", Scopes: []string{ScopeModelsRead}},
			{Token: "a", Scopes: []string{ScopeChatInvoke}},
		},
	})
	if err == nil {
		t.Fatal("NewAuthorizer() error = nil, want duplicate-token validation error")
	}
}

func TestAuthorizer_AuthenticatesScopedGrant(t *testing.T) {
	authz, err := NewAuthorizer(Config{
		Tokens: []TokenRule{
			{
				Token:             "secret",
				Scopes:            []string{ScopeChatInvoke, ScopeSessionsRead},
				WorkspacePrefixes: []string{"repo-"},
				AllowedProviders:  []string{"Codex"},
				AllowedModes:      []string{"Balanced"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	grant, ok := authz.AuthenticateRequest(req)
	if !ok {
		t.Fatal("AuthenticateRequest() = false, want true")
	}
	if grant.AllowsScope(ScopeModelsRead) {
		t.Fatal("AllowsScope(models:read) = true, want false")
	}
	if !grant.AllowsScope(ScopeChatInvoke) {
		t.Fatal("AllowsScope(chat:invoke) = false, want true")
	}
	if !grant.AllowsWorkspace("repo-main") {
		t.Fatal("AllowsWorkspace(repo-main) = false, want true")
	}
	if grant.AllowsWorkspace("other-main") {
		t.Fatal("AllowsWorkspace(other-main) = true, want false")
	}
	if !grant.AllowsProvider("codex") {
		t.Fatal("AllowsProvider(codex) = false, want true")
	}
	if grant.AllowsProvider("gemini") {
		t.Fatal("AllowsProvider(gemini) = true, want false")
	}
	if !grant.AllowsMode("balanced") {
		t.Fatal("AllowsMode(balanced) = false, want true")
	}
	if grant.AllowsMode("power") {
		t.Fatal("AllowsMode(power) = true, want false")
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_auth.json")
	content := []byte(`{
  "tokens": [
    {
      "token": "secret",
      "scopes": ["models:read", "chat:invoke"],
      "workspace_prefixes": ["repo-"],
      "allowed_providers": ["codex"],
      "allowed_modes": ["balanced"]
    }
  ]
}`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	authz, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if _, ok := authz.AuthenticateRequest(req); !ok {
		t.Fatal("AuthenticateRequest() = false, want true")
	}
}
