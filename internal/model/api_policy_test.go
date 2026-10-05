package model

import (
	"context"
	"testing"

	"github.com/makewand/makewand/internal/testfixture"

	"github.com/makewand/makewand/internal/config"
)

func policyConfig(t *testing.T) *config.Config {
	t.Helper()
	testfixture.ClearProviderEnv(t)
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Setenv("MAKEWAND_REMOTE_URL", "")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "")
	cfg := config.DefaultConfig()
	cfg.ClaudeAPIKey = "test-anthropic-key"
	cfg.GeminiAPIKey = "test-gemini-key"
	cfg.OpenAIAPIKey = "test-openai-key"
	return cfg
}

func TestSubscriptionOnlyOmitsCloudAPIsAndDynamicFactories(t *testing.T) {
	t.Setenv("MAKEWAND_API_POLICY", "subscription_only")
	cfg := policyConfig(t)
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "claude-api", "gemini", "gemini-api", "openai", "codex"} {
		if _, err := r.Get(name); err == nil {
			t.Fatalf("cloud provider %s enabled without opt-in", name)
		}
	}
	// Changing the original configuration after construction must not enable the
	// factory closures. ChatBest visits dynamic mode routes as well as the pool.
	cfg.APIPolicy = config.APIPolicyAllowPaid
	cfg.ClaudeAPIKey = "changed"
	r.SetMode(ModeBalanced)
	_, _, _, err = r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "test"}}, "")
	if err == nil {
		t.Fatal("dynamic factory bypassed subscription-only policy")
	}
}

func TestPaidAPIRequiresExplicitOptIn(t *testing.T) {
	for _, policy := range []string{"allow_paid", "subscription_only", "misspelled-policy", ""} {
		t.Run(policy, func(t *testing.T) {
			cfg := policyConfig(t)
			cfg.APIPolicy = config.APIPolicyAllowPaid
			t.Setenv("MAKEWAND_API_POLICY", policy)
			r, err := NewRouter(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"claude", "gemini", "openai"} {
				_, err := r.Get(name)
				if (err == nil) != (policy == "allow_paid") {
					t.Fatalf("policy %q provider %s: %v", policy, name, err)
				}
			}
		})
	}
}

func TestSubscriptionOnlyKeepsCLIAndOmitsPaidSiblingAndCustomAdapter(t *testing.T) {
	t.Setenv("MAKEWAND_API_POLICY", "subscription_only")
	cfg := policyConfig(t)
	bin := writeFixtureCLI(t, "provider")
	cfg.CLIs = []config.CLITool{{Name: "claude", BinPath: bin}}
	cfg.CustomProviders = []config.CustomProvider{{Name: "paid-custom", Command: bin, Access: "api"}, {Name: "local-custom", Command: bin, Access: "local"}}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "local-custom"} {
		if _, err := r.Get(name); err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
	}
	for _, name := range []string{"claude-api", "paid-custom"} {
		if _, err := r.Get(name); err == nil {
			t.Fatalf("paid fallback %s enabled", name)
		}
	}
	t.Setenv("MAKEWAND_API_POLICY", "allow_paid")
	r, err = NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-api", "paid-custom"} {
		if _, err := r.Get(name); err != nil {
			t.Fatalf("explicitly enabled %s unavailable: %v", name, err)
		}
	}
}
