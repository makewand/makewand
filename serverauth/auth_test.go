package serverauth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

	_, err = NewAuthorizer(Config{
		Tokens: []TokenRule{
			{Token: "a", Scopes: []string{ScopeModelsRead}, MaxRequestsPerHour: -1},
		},
	})
	if err == nil {
		t.Fatal("NewAuthorizer() error = nil, want negative quota validation error")
	}
}

func TestAuthorizer_AuthenticatesScopedGrant(t *testing.T) {
	authz, err := NewAuthorizer(Config{
		Tokens: []TokenRule{
			{
				ID:                "runner",
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
	if got := grant.TokenID(); got != "runner" {
		t.Fatalf("TokenID() = %q, want %q", got, "runner")
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

func TestAuthorizer_RejectsRevokedAndExpiredTokens(t *testing.T) {
	expiredAt := time.Now().UTC().Add(-time.Minute)
	authz, err := NewAuthorizer(Config{
		Tokens: []TokenRule{
			{
				Token:     "revoked",
				Scopes:    []string{ScopeModelsRead},
				Revoked:   true,
				ExpiresAt: time.Now().UTC().Add(time.Hour),
			},
			{
				Token:     "expired",
				Scopes:    []string{ScopeModelsRead},
				ExpiresAt: expiredAt,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	if _, ok := authz.AuthenticateHeader("Bearer revoked"); ok {
		t.Fatal("AuthenticateHeader(revoked) = true, want false")
	}
	if _, ok := authz.AuthenticateHeader("Bearer expired"); ok {
		t.Fatal("AuthenticateHeader(expired) = true, want false")
	}
}

func TestNewAuthorizer_DefaultTokenIDIsStableAndUnique(t *testing.T) {
	authz, err := NewAuthorizer(Config{
		Tokens: []TokenRule{
			{Token: "alpha", Scopes: []string{ScopeModelsRead}},
			{Token: "beta", Scopes: []string{ScopeModelsRead}},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	alpha, ok := authz.AuthenticateHeader("Bearer alpha")
	if !ok {
		t.Fatal("AuthenticateHeader(alpha) = false, want true")
	}
	beta, ok := authz.AuthenticateHeader("Bearer beta")
	if !ok {
		t.Fatal("AuthenticateHeader(beta) = false, want true")
	}
	if alpha.TokenID() == "" {
		t.Fatal("alpha.TokenID() = empty, want derived ID")
	}
	if alpha.TokenID() == beta.TokenID() {
		t.Fatalf("derived token IDs must be unique; both were %q", alpha.TokenID())
	}
}

func TestGrant_AllowRequestAt_ResetsHourly(t *testing.T) {
	authz, err := NewAuthorizer(Config{
		Tokens: []TokenRule{
			{
				Token:              "alpha",
				Scopes:             []string{ScopeModelsRead},
				MaxRequestsPerHour: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	grant, ok := authz.AuthenticateHeader("Bearer alpha")
	if !ok {
		t.Fatal("AuthenticateHeader(alpha) = false, want true")
	}

	firstHour := time.Date(2026, 3, 23, 10, 15, 0, 0, time.UTC)
	if !grant.AllowRequestAt(firstHour) {
		t.Fatal("AllowRequestAt(firstHour) = false, want true")
	}
	if grant.AllowRequestAt(firstHour.Add(10 * time.Minute)) {
		t.Fatal("AllowRequestAt(same hour) = true, want false")
	}
	if !grant.AllowRequestAt(firstHour.Add(time.Hour)) {
		t.Fatal("AllowRequestAt(next hour) = false, want true")
	}
}
