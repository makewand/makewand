package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_EnvOverridesWhenConfigMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "ant-test-key")
	t.Setenv("GEMINI_API_KEY", "gem-test-key")
	t.Setenv("OPENAI_API_KEY", "openai-test-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.ClaudeAPIKey != "ant-test-key" {
		t.Fatalf("ClaudeAPIKey = %q, want %q", cfg.ClaudeAPIKey, "ant-test-key")
	}
	if cfg.GeminiAPIKey != "gem-test-key" {
		t.Fatalf("GeminiAPIKey = %q, want %q", cfg.GeminiAPIKey, "gem-test-key")
	}
	if cfg.OpenAIAPIKey != "openai-test-key" {
		t.Fatalf("OpenAIAPIKey = %q, want %q", cfg.OpenAIAPIKey, "openai-test-key")
	}
	if !cfg.HasAnyModel() {
		t.Fatal("HasAnyModel() = false, want true")
	}
}

func TestLoad_ParseErrorStillAppliesEnvOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENAI_API_KEY", "openai-env-key")

	cfgDir := filepath.Join(home, ".config", "makewand")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(path, []byte("{invalid-json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want parse error")
	}
	if cfg.OpenAIAPIKey != "openai-env-key" {
		t.Fatalf("OpenAIAPIKey = %q, want %q", cfg.OpenAIAPIKey, "openai-env-key")
	}
	if cfg.DefaultModel != "claude" {
		t.Fatalf("DefaultModel = %q, want %q", cfg.DefaultModel, "claude")
	}
}

func TestSave_StripsEnvSourcedKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENAI_API_KEY", "openai-env-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(data) == "" {
		t.Fatal("config file is empty")
	}
	if strings.Contains(string(data), "openai-env-key") {
		t.Fatal("config file should not persist env-sourced OPENAI_API_KEY")
	}
}

func TestConfigDir_UsesEnvOverride(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "custom-makewand-config")
	t.Setenv("HOME", home)
	t.Setenv("MAKEWAND_CONFIG_DIR", custom)

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}
	if dir != custom {
		t.Fatalf("ConfigDir() = %q, want %q", dir, custom)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("Stat(%s): %v", custom, err)
	}

	path, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error: %v", err)
	}
	wantPath := filepath.Join(custom, "config.json")
	if path != wantPath {
		t.Fatalf("ConfigPath() = %q, want %q", path, wantPath)
	}
}
