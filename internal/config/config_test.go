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

func TestHasAnyModel_WithCustomProvider(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "private-llm.sh")
	//nolint:gosec // G306: test fixture script must be executable.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", bin, err)
	}

	cfg := DefaultConfig()
	cfg.ClaudeAPIKey = ""
	cfg.GeminiAPIKey = ""
	cfg.OpenAIAPIKey = ""
	cfg.CLIs = nil
	cfg.CustomProviders = []CustomProvider{
		{Name: "private-llm", Command: bin},
	}

	if !cfg.HasAnyModel() {
		t.Fatal("HasAnyModel() = false, want true when custom providers are configured")
	}
}

func TestHasAnyModel_InvalidCustomProviderNotCounted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ClaudeAPIKey = ""
	cfg.GeminiAPIKey = ""
	cfg.OpenAIAPIKey = ""
	cfg.CLIs = nil
	cfg.CustomProviders = []CustomProvider{
		{Name: "broken-private-llm", Command: "/path/does/not/exist"},
	}

	if cfg.HasAnyModel() {
		t.Fatal("HasAnyModel() = true, want false when all custom providers are unusable")
	}
}

func TestIsCustomProviderUsable_WithExecutablePath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "provider.sh")
	//nolint:gosec // G306: test fixture script must be executable.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", bin, err)
	}

	if !IsCustomProviderUsable(CustomProvider{Name: "private", Command: bin}) {
		t.Fatal("IsCustomProviderUsable() = false, want true for executable file")
	}
}

func TestEffectiveCustomProviderPromptMode(t *testing.T) {
	tests := []struct {
		name string
		cp   CustomProvider
		want string
	}{
		{name: "empty keeps legacy", cp: CustomProvider{}, want: CustomPromptModeLegacy},
		{name: "stdin", cp: CustomProvider{PromptMode: "stdin"}, want: CustomPromptModeStdin},
		{name: "arg", cp: CustomProvider{PromptMode: "arg"}, want: CustomPromptModeArg},
		{name: "unknown falls back to legacy", cp: CustomProvider{PromptMode: "weird"}, want: CustomPromptModeLegacy},
	}

	for _, tt := range tests {
		if got := EffectiveCustomProviderPromptMode(tt.cp); got != tt.want {
			t.Fatalf("%s: EffectiveCustomProviderPromptMode() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCustomProviderUsesShellAdapter(t *testing.T) {
	if !CustomProviderUsesShellAdapter(CustomProvider{Command: "/bin/sh", Args: []string{"-c", "echo ok"}}) {
		t.Fatal("CustomProviderUsesShellAdapter(sh -c) = false, want true")
	}
	if CustomProviderUsesShellAdapter(CustomProvider{Command: "/usr/local/bin/private-llm", Args: []string{"--mode", "fast"}}) {
		t.Fatal("CustomProviderUsesShellAdapter(normal binary) = true, want false")
	}
}

func TestDefaultConfig_ApprovalModeIsManual(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ApprovalMode != ApprovalModeManual {
		t.Fatalf("ApprovalMode = %q, want %q", cfg.ApprovalMode, ApprovalModeManual)
	}
}

func TestDefaultConfig_UsageModeIsBalanced(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.UsageMode != UsageModeBalanced {
		t.Fatalf("UsageMode = %q, want %q", cfg.UsageMode, UsageModeBalanced)
	}
}

func TestNormalizeUsageMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "", want: UsageModeBalanced},
		{input: "fast", want: UsageModeFast},
		{input: " BALANCED ", want: UsageModeBalanced},
		{input: "POWER", want: UsageModePower},
		{input: "legacy", want: UsageModeBalanced},
	}

	for _, tt := range tests {
		if got := NormalizeUsageMode(tt.input); got != tt.want {
			t.Fatalf("NormalizeUsageMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLoad_LegacyUsageModeDefaultsToBalanced(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing", data: `{"language":"en"}`},
		{name: "empty", data: `{"usage_mode":""}`},
		{name: "invalid", data: `{"usage_mode":"legacy"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgDir := t.TempDir()
			t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)
			t.Setenv("PATH", t.TempDir())

			path := filepath.Join(cfgDir, "config.json")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatalf("WriteFile(%s): %v", path, err)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.UsageMode != UsageModeBalanced {
				t.Fatalf("UsageMode = %q, want %q", cfg.UsageMode, UsageModeBalanced)
			}
		})
	}
}

// TestLoadWithOptions_SkipCLIDetection verifies that untrusted-repository mode's
// config load (LoadOptions{SkipCLIDetection:true}) never execs a subscription
// CLI's `--version` probe, while the trusted default (Load) still detects it.
func TestLoadWithOptions_SkipCLIDetection(t *testing.T) {
	binDir := t.TempDir()

	// Fake `claude` on PATH: writes a sentinel when its --version probe runs.
	// The probe inherits the (restricted) test PATH, so create the sentinel via a
	// shell builtin + redirection rather than an external command like `touch`.
	sentinel := filepath.Join(binDir, "claude_probe_ran")
	script := filepath.Join(binDir, "claude")
	body := "#!/bin/sh\nprintf 'probe\\n' > \"" + sentinel + "\"\nprintf 'claude 1.2.3\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture must be executable.
		t.Fatalf("WriteFile(script): %v", err)
	}

	t.Run("skip does not exec probe", func(t *testing.T) {
		t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
		t.Setenv("PATH", binDir)
		_ = os.Remove(sentinel)

		cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
		if err != nil {
			t.Fatalf("LoadWithOptions(SkipCLIDetection) error: %v", err)
		}
		if _, statErr := os.Stat(sentinel); statErr == nil {
			t.Fatalf("CLI --version probe was executed despite SkipCLIDetection (sentinel %q exists)", sentinel)
		}
		if len(cfg.CLIs) != 0 {
			t.Fatalf("cfg.CLIs = %+v, want none when detection skipped", cfg.CLIs)
		}
	})

	t.Run("trusted default detects CLIs", func(t *testing.T) {
		t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
		t.Setenv("PATH", binDir)
		_ = os.Remove(sentinel)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if _, statErr := os.Stat(sentinel); statErr != nil {
			t.Fatalf("trusted Load did not exec the CLI --version probe (sentinel %q missing): %v", sentinel, statErr)
		}
		if !cfg.HasCLI("claude") {
			t.Fatalf("cfg.HasCLI(claude) = false, want true; CLIs = %+v", cfg.CLIs)
		}
	})
}

func TestNormalizeApprovalMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "", want: ApprovalModeManual},
		{input: "manual", want: ApprovalModeManual},
		{input: "safe", want: ApprovalModeSafe},
		{input: "SAFE", want: ApprovalModeSafe},
		{input: "autopilot", want: ApprovalModeAuto},
		{input: "weird", want: ApprovalModeManual},
	}

	for _, tt := range tests {
		if got := NormalizeApprovalMode(tt.input); got != tt.want {
			t.Fatalf("NormalizeApprovalMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
