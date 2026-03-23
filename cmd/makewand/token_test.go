package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/serverauth"
)

func TestIssueTokenRule_AssignsStableID(t *testing.T) {
	cfg := serverauth.Config{}
	id, err := issueTokenRule(&cfg, serverauth.TokenRule{
		Token:  "secret",
		Scopes: []string{serverauth.ScopeModelsRead},
	})
	if err != nil {
		t.Fatalf("issueTokenRule: %v", err)
	}
	if id == "" {
		t.Fatal("issued token ID = empty")
	}
	if len(cfg.Tokens) != 1 {
		t.Fatalf("len(cfg.Tokens) = %d, want 1", len(cfg.Tokens))
	}
	if cfg.Tokens[0].ID != id {
		t.Fatalf("cfg.Tokens[0].ID = %q, want %q", cfg.Tokens[0].ID, id)
	}
}

func TestRevokeTokenRule_MarksTokenRevoked(t *testing.T) {
	cfg := serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "runner",
				Token:  "secret",
				Scopes: []string{serverauth.ScopeModelsRead},
			},
		},
	}
	if err := revokeTokenRule(&cfg, "runner"); err != nil {
		t.Fatalf("revokeTokenRule: %v", err)
	}
	if !cfg.Tokens[0].Revoked {
		t.Fatal("cfg.Tokens[0].Revoked = false, want true")
	}
}

func TestParseCSVOrDefault(t *testing.T) {
	fallback := []string{"a", "b"}
	got := parseCSVOrDefault("", fallback)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("parseCSVOrDefault fallback = %v", got)
	}
	got = parseCSVOrDefault("x, y", fallback)
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("parseCSVOrDefault override = %v", got)
	}
}

func TestResolveManagedAuthConfigPath_DefaultsToConfigDir(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	t.Setenv("MAKEWAND_SERVER_AUTH_CONFIG", "")

	got, err := resolveManagedAuthConfigPath("")
	if err != nil {
		t.Fatalf("resolveManagedAuthConfigPath: %v", err)
	}
	want := filepath.Join(cfgDir, "server_auth.json")
	if got != want {
		t.Fatalf("resolveManagedAuthConfigPath() = %q, want %q", got, want)
	}
}

func TestSanitizedTokenRules_HidesRawToken(t *testing.T) {
	views := sanitizedTokenRules([]serverauth.TokenRule{
		{
			Token:  "secret",
			Scopes: []string{serverauth.ScopeModelsRead},
		},
	})
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1", len(views))
	}
	data, err := json.Marshal(views[0])
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(data) == "" {
		t.Fatal("json output = empty")
	}
	if string(data) != "" && string(data) == "null" {
		t.Fatal("json output = null")
	}
	if string(data) != "" && string(data) == "{}" {
		t.Fatal("json output = {}, want redacted metadata")
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("json output leaked token: %s", string(data))
	}
}
