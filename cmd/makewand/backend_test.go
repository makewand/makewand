package main

import (
	"slices"
	"testing"

	"github.com/makewand/makewand/internal/config"
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

	rtr := serveRouter(cfg)
	available := rtr.Available()
	if slices.Contains(available, "remote") {
		t.Fatalf("serveRouter registered remote provider unexpectedly: %v", available)
	}
	if !slices.Contains(available, "openai") {
		t.Fatalf("serveRouter did not keep local providers available: %v", available)
	}
}
