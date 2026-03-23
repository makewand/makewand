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

func TestResolveServeAuditPath(t *testing.T) {
	t.Setenv("MAKEWAND_SERVER_AUDIT_LOG", "")
	if got := resolveServeAuditPath("", "/tmp/server"); got != "" {
		t.Fatalf("resolveServeAuditPath() = %q, want empty", got)
	}

	t.Setenv("MAKEWAND_SERVER_AUDIT_LOG", "1")
	if got := resolveServeAuditPath("", "/tmp/server"); got != "/tmp/server/audit.jsonl" {
		t.Fatalf("resolveServeAuditPath(env=1) = %q", got)
	}

	t.Setenv("MAKEWAND_SERVER_AUDIT_LOG", "/var/log/makewand-audit.jsonl")
	if got := resolveServeAuditPath("", "/tmp/server"); got != "/var/log/makewand-audit.jsonl" {
		t.Fatalf("resolveServeAuditPath(env=path) = %q", got)
	}

	if got := resolveServeAuditPath("/custom/audit.jsonl", "/tmp/server"); got != "/custom/audit.jsonl" {
		t.Fatalf("resolveServeAuditPath(flag) = %q", got)
	}
}
