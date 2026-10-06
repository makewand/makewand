package config

import (
	"github.com/makewand/makewand/internal/testfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadInvalidPolicyLocksProvidersBeforeDiscovery(t *testing.T) {
	for _, data := range []string{
		`{"active_providers":[]`, `{"active_providers":[]}X`, `[]`, `null`, `"config"`, `1`, `true`,
		`{"enabled_providers":[]}`, `{"enabled_providers":{"claude":"false"}}`, `{"enabled_providers":{"claude":null}}`,
		`{"active_providers":"claude"}`, `{"active_providers":["claude",3]}`, `{"active_providers":[null]}`,
		`{"local_model_enabled":"false"}`, `{"local_model_enabled":1}`,
	} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			bin := t.TempDir()
			marker := filepath.Join(bin, "called")
			testfixture.WriteCLIAt(t, bin, "claude", testfixture.Spec{Mode: "probe", Sentinel: marker, Stdout: "fixture 1.0\n"})
			t.Setenv("MAKEWAND_CONFIG_DIR", dir)
			t.Setenv("PATH", bin)
			t.Setenv("MAKEWAND_API_POLICY", "allow_paid")
			t.Setenv("MAKEWAND_ENABLE_PROVIDERS", "claude,codex,agy,local,custom")
			t.Setenv("ANTHROPIC_API_KEY", "offline-fixture")
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if !IsFatalLoadError(err) {
				t.Fatalf("expected fatal policy error, got %v", err)
			}
			// Ignoring the error and mutating an SDK snapshot must not resurrect
			// providers or paid admission after the policy was unreadable.
			cfg.ClaudeAPIKey = "offline-mutated-key"
			cfg.APIPolicy = APIPolicyAllowPaid
			for _, provider := range []string{"claude", "claude-api", "codex", "agy", "local", "custom"} {
				if cfg.IsProviderEnabled(provider) {
					t.Fatalf("%s enabled despite invalid policy", provider)
				}
			}
			if cfg.HasAnyModel() || cfg.PaidAPIAllowed() || len(cfg.CLIs) != 0 {
				t.Fatal("invalid policy permits models, spend or detected CLIs")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("CLI discovery ran: %v", err)
			}
			if err := Save(cfg); err == nil {
				t.Fatal("Save accepted locked configuration")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != data {
				t.Fatalf("invalid config overwritten: %v", err)
			}
		})
	}
}

func TestSaveRejectsLatestInvalidProviderControls(t *testing.T) {
	for _, invalid := range []string{`{"enabled_providers":{"claude":"false"}}`, `{"active_providers":[3]}`, `{"local_model_enabled":1}`} {
		t.Run(invalid, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("MAKEWAND_CONFIG_DIR", dir)
			cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			// Another frontend updated its fields after the Go snapshot loaded.
			if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Language = "zh"
			if err := Save(cfg); err == nil {
				t.Fatal("preference update persisted invalid merged authorization fields")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != invalid {
				t.Fatal("failed save changed existing policy")
			}
		})
	}
}

func TestLoadUnreadablePolicyAndMissingOptionalFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if !IsFatalLoadError(err) || cfg.IsProviderEnabled("claude") {
		t.Fatalf("unreadable policy not locked: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if err != nil || !cfg.IsProviderEnabled("claude") {
		t.Fatalf("missing optional config should retain normal defaults: %v", err)
	}
}

func TestLoadCredentialWarningRetainsIndependentEnvironmentFields(t *testing.T) {
	for _, data := range []string{`{`, `[]`, `null`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("MAKEWAND_CONFIG_DIR", dir)
			t.Setenv("ANTHROPIC_API_KEY", "offline-env-key")
			t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
			t.Setenv("ANTHROPIC_MODEL", "offline-env-model")
			t.Setenv("MAKEWAND_API_POLICY", "subscription_only")
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"claude_model":"flat-model","api":{"codex":{"api_key":"offline-nested-key"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			keysPath := filepath.Join(dir, "api_keys.json")
			if err := os.WriteFile(keysPath, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
			if err == nil || IsFatalLoadError(err) || !strings.Contains(err.Error(), "api_keys.json") {
				t.Fatalf("expected optional credentials warning, got %v", err)
			}
			if cfg.ClaudeAPIKey != "offline-env-key" || cfg.ClaudeModel != "offline-env-model" || cfg.ClaudeBaseURL != "http://127.0.0.1:1" || cfg.OpenAIAPIKey != "offline-nested-key" {
				t.Fatal("broken lower-priority source suppressed valid independent fields")
			}
			if cfg.PaidAPIAllowed() {
				t.Fatal("reading an environment key opted into paid API use")
			}
			if err := Save(cfg); err != nil {
				t.Fatalf("valid policy with credential warning cannot save preferences: %v", err)
			}
			after, err := os.ReadFile(keysPath)
			if err != nil || string(after) != data {
				t.Fatal("preference save changed damaged credential file")
			}
		})
	}
}
