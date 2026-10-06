package config

import (
	"encoding/json"
	"github.com/makewand/makewand/internal/testfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearProviderSettingsEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "MAKEWAND_ENABLE_") || strings.HasPrefix(name, "MAKEWAND_DISABLE_") || strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_BASE_URL") || strings.HasSuffix(name, "_MODEL") {
			t.Setenv(name, "")
		}
	}
}

// Both runtimes consume this fixture. The expectations are policy decisions,
// not values derived from either implementation.
func TestSharedProviderEnablementContract(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/shared_config_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name        string
		Config      json.RawMessage
		Env         map[string]string
		Expected    map[string]bool
		ExpectError bool `json:"expect_error"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			clearProviderSettingsEnv(t)
			for name, value := range tc.Env {
				t.Setenv(name, value)
			}
			dir := t.TempDir()
			t.Setenv("MAKEWAND_CONFIG_DIR", dir)
			if err := os.WriteFile(filepath.Join(dir, "config.json"), tc.Config, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
			if tc.ExpectError {
				if !IsFatalLoadError(err) || cfg.IsProviderEnabled("claude") {
					t.Fatalf("invalid authorization values did not lock policy: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for name, expected := range tc.Expected {
				if cfg.IsProviderEnabled(name) != expected {
					t.Errorf("%s enabled=%v, want %v", name, cfg.IsProviderEnabled(name), expected)
				}
			}
		})
	}
}

func TestDisabledProviderIsNotProbedDuringLoad(t *testing.T) {
	clearProviderSettingsEnv(t)
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	sentinel := filepath.Join(dir, "executed")
	for _, name := range []string{"claude", "codex", "gemini", "agy"} {
		testfixture.WriteCLIAt(t, dir, name, testfixture.Spec{Mode: "probe", Sentinel: sentinel, Stdout: "fixture 1.0\n"})
	}
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"enabled_providers":{"claude":false,"codex":false,"gemini":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CLIs) != 0 {
		t.Fatalf("disabled CLI detected: %+v", cfg.CLIs)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("disabled CLI probed: %v", err)
	}
	// Positive control proves these fixture CLIs can execute.
	t.Setenv("MAKEWAND_ENABLE_CLAUDE", "1")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CLIs) != 1 || cfg.CLIs[0].Name != "claude" {
		t.Fatalf("enabled detection: %+v", cfg.CLIs)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("positive control did not execute", err)
	}
}

func TestAPIConfigPrecedenceAndPrivateSave(t *testing.T) {
	clearProviderSettingsEnv(t)
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	configData := `{"claude_api_key":"flat-claude","claude_model":"flat-model","claude_base_url":"https://flat.invalid","api":{"claude":{"api_key":"nested-claude","model":"nested-model"},"gemini":{"api_key":"nested-gemini","model":"nested-gemini-model"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configData), 0600); err != nil {
		t.Fatal(err)
	}
	fileData := `{"anthropic":{"api_key":"alias-key","model":"alias-model"},"claude":{"api_key":"file-key","base_url":"https://file.invalid"},"openai":{"api_key":"file-openai","model":"file-openai-model"}}`
	if err := os.WriteFile(filepath.Join(dir, "api_keys.json"), []byte(fileData), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_MODEL", "env-model")
	t.Setenv("ANTHROPIC_BASE_URL", "https://env.invalid")
	t.Setenv("GOOGLE_API_KEY", "env-google")
	cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeAPIKey != "file-key" || cfg.ClaudeModel != "env-model" || cfg.ClaudeBaseURL != "https://env.invalid" || cfg.GeminiAPIKey != "env-google" || cfg.GeminiModel != "nested-gemini-model" || cfg.OpenAIAPIKey != "file-openai" || cfg.OpenAIModel != "file-openai-model" {
		t.Fatalf("precedence failed: %+v", cfg)
	}
	if cfg.HasAnyModel() {
		t.Fatal("file/environment credentials authorized paid API use")
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, external := range []string{"file-key", "file-openai", "env-google", "env-model", "env.invalid"} {
		if strings.Contains(string(saved), external) {
			t.Errorf("external API field persisted: %s", external)
		}
	}
	if !strings.Contains(string(saved), "flat-model") || !strings.Contains(string(saved), "flat.invalid") {
		t.Fatal("disk fallback model/URL overwritten")
	}
	// Explicit changes made by a frontend after Load are owned by that frontend.
	cfg.ClaudeModel = "frontend-model"
	cfg.ClaudeBaseURL = "https://frontend.invalid"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err = os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "frontend-model") || !strings.Contains(string(saved), "frontend.invalid") {
		t.Fatal("explicit model/URL changes were discarded")
	}
}

func TestBlankFlatAPIFieldsUseNestedFallback(t *testing.T) {
	clearProviderSettingsEnv(t)
	dir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	data := `{"claude_api_key":"  ","claude_model":"  ","claude_base_url":"  ","api":{"claude":{"api_key":"nested-key","model":"nested-model","base_url":"https://nested.invalid"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeAPIKey != "nested-key" || cfg.ClaudeModel != "nested-model" || cfg.ClaudeBaseURL != "https://nested.invalid" {
		t.Fatal("blank fields blocked nested fallback")
	}
}
