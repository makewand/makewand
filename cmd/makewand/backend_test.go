package main

import (
	"slices"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

func TestHasUsableBackend_AllowsRemote(t *testing.T) {
	t.Setenv("MAKEWAND_REMOTE_URL", "http://127.0.0.1:8080")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "secret")

	if !hasUsableBackend(config.DefaultConfig()) {
		t.Fatal("hasUsableBackend should allow remote backend configuration")
	}
}

func TestServeRouter_IgnoresRemoteBackendEnv(t *testing.T) {
	t.Setenv("MAKEWAND_REMOTE_URL", "http://127.0.0.1:8080")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "secret")

	cfg := config.DefaultConfig()
	cfg.OpenAIAPIKey = "openai-test-key"

	rtr, err := serveRouter(cfg, model.RepoTrustTrusted)
	if err != nil {
		t.Fatalf("serveRouter() error = %v", err)
	}
	available := rtr.Available()
	if slices.Contains(available, "remote") {
		t.Fatalf("serveRouter registered remote provider unexpectedly: %v", available)
	}
	if !slices.Contains(available, "openai") {
		t.Fatalf("serveRouter did not keep local providers available: %v", available)
	}
}

// TestServeRouter_HonorsUntrustedTrust verifies that serve constructs its router
// with the resolved --repo-trust value, so untrusted mode is honored end-to-end
// (the HTTP facade routes through the Router's gated pipeline).
func TestServeRouter_HonorsUntrustedTrust(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.OpenAIAPIKey = "openai-test-key"

	rtr, err := serveRouter(cfg, model.RepoTrustUntrusted)
	if err != nil {
		t.Fatalf("serveRouter() error = %v", err)
	}
	if rtr.RepoTrust() != model.RepoTrustUntrusted {
		t.Fatalf("serveRouter RepoTrust() = %v, want %v", rtr.RepoTrust(), model.RepoTrustUntrusted)
	}

	trusted, err := serveRouter(cfg, model.RepoTrustTrusted)
	if err != nil {
		t.Fatalf("serveRouter(trusted) error = %v", err)
	}
	if trusted.RepoTrust() != model.RepoTrustTrusted {
		t.Fatalf("serveRouter RepoTrust() = %v, want %v", trusted.RepoTrust(), model.RepoTrustTrusted)
	}
}
