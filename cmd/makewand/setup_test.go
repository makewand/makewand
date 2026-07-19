package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

func TestDefaultConfigEnablesBalancedModeRouting(t *testing.T) {
	cfg := config.DefaultConfig()
	r, err := model.NewRouterFromConfig(model.RouterConfig{UsageMode: cfg.UsageMode})
	if err != nil {
		t.Fatalf("NewRouterFromConfig() error = %v", err)
	}

	if !r.ModeSet() {
		t.Fatal("ModeSet() = false, want true for the default config")
	}
	if r.Mode() != model.ModeBalanced {
		t.Fatalf("Mode() = %v, want %v", r.Mode(), model.ModeBalanced)
	}
}

func TestApplySetupUsageMode(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		requested  string
		want       string
		wantErr    bool
	}{
		{name: "legacy empty defaults to balanced", configured: "", want: config.UsageModeBalanced},
		{name: "valid configured mode is retained", configured: "power", want: config.UsageModePower},
		{name: "configured mode is canonicalized", configured: " FAST ", want: config.UsageModeFast},
		{name: "flag overrides configured mode", configured: "power", requested: "balanced", want: config.UsageModeBalanced},
		{name: "flag is canonicalized", configured: "balanced", requested: " POWER ", want: config.UsageModePower},
		{name: "invalid flag is rejected", configured: "balanced", requested: "legacy", want: config.UsageModeBalanced, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{UsageMode: tt.configured}
			err := applySetupUsageMode(cfg, tt.requested)
			if (err != nil) != tt.wantErr {
				t.Fatalf("applySetupUsageMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if cfg.UsageMode != tt.want {
				t.Fatalf("UsageMode = %q, want %q", cfg.UsageMode, tt.want)
			}
		})
	}
}

func TestSetupCmdPersistsRoutingMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "legacy empty becomes balanced", want: config.UsageModeBalanced},
		{name: "mode flag is persisted", args: []string{"--mode", "power"}, want: config.UsageModePower},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgDir := t.TempDir()
			t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
			t.Setenv("PATH", t.TempDir())
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("GEMINI_API_KEY", "")
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("MAKEWAND_REMOTE_URL", "")
			t.Setenv("MAKEWAND_REMOTE_TOKEN", "")

			path := filepath.Join(cfgDir, "config.json")
			if err := os.WriteFile(path, []byte(`{"usage_mode":""}`), 0o600); err != nil {
				t.Fatalf("WriteFile(%s): %v", path, err)
			}

			cmd := setupCmd()
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("setup command error: %v", err)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			var saved config.Config
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatalf("Unmarshal saved config: %v", err)
			}
			if saved.UsageMode != tt.want {
				t.Fatalf("saved UsageMode = %q, want %q", saved.UsageMode, tt.want)
			}
		})
	}
}

func TestSetupCmdInvalidModeDoesNotRewriteConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("MAKEWAND_REMOTE_URL", "")
	t.Setenv("MAKEWAND_REMOTE_TOKEN", "")

	path := filepath.Join(cfgDir, "config.json")
	original := []byte("{\n  \"usage_mode\": \"power\"\n}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	cmd := setupCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--mode", "legacy"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("setup --mode legacy succeeded, want validation error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(after) != string(original) {
		t.Fatalf("invalid setup mode rewrote config:\n%s", after)
	}
}
