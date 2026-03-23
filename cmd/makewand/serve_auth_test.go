package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadServeAuthorizer_UsesConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_auth.json")
	content := []byte(`{
  "tokens": [
    {
      "token": "secret",
      "scopes": ["models:read", "chat:invoke"]
    }
  ]
}`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	authz, err := loadServeAuthorizer("", path)
	if err != nil {
		t.Fatalf("loadServeAuthorizer: %v", err)
	}
	if authz == nil {
		t.Fatal("loadServeAuthorizer() = nil, want authorizer")
	}
}

func TestLoadServeAuthorizer_UsesLegacyTokenFallback(t *testing.T) {
	authz, err := loadServeAuthorizer("secret", "")
	if err != nil {
		t.Fatalf("loadServeAuthorizer: %v", err)
	}
	if authz == nil {
		t.Fatal("loadServeAuthorizer() = nil, want authorizer")
	}
}
