package model

import (
	"testing"

	"github.com/makewand/makewand/internal/config"
)

// TestNewRouterWithTrust verifies that the trust level is applied at construction
// so untrusted mode is known before any background work (quota refresh) runs.
func TestNewRouterWithTrust(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.OpenAIAPIKey = "openai-test-key"

	untrusted, err := NewRouterWithTrust(cfg, RepoTrustUntrusted)
	if err != nil {
		t.Fatalf("NewRouterWithTrust(untrusted) error = %v", err)
	}
	if untrusted.RepoTrust() != RepoTrustUntrusted {
		t.Fatalf("NewRouterWithTrust(untrusted).RepoTrust() = %v, want %v", untrusted.RepoTrust(), RepoTrustUntrusted)
	}

	trusted, err := NewRouterWithTrust(cfg, RepoTrustTrusted)
	if err != nil {
		t.Fatalf("NewRouterWithTrust(trusted) error = %v", err)
	}
	if trusted.RepoTrust() != RepoTrustTrusted {
		t.Fatalf("NewRouterWithTrust(trusted).RepoTrust() = %v, want %v", trusted.RepoTrust(), RepoTrustTrusted)
	}
}

// TestNewRouterDefaultsToTrusted guards that the existing NewRouter entry point
// (and its ~12 callers) keeps the trusted default byte-for-byte.
func TestNewRouterDefaultsToTrusted(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.OpenAIAPIKey = "openai-test-key"

	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter error = %v", err)
	}
	if r.RepoTrust() != RepoTrustTrusted {
		t.Fatalf("NewRouter(cfg).RepoTrust() = %v, want %v", r.RepoTrust(), RepoTrustTrusted)
	}
}
