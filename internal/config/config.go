package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config holds the application configuration.
type Config struct {
	// Model API keys
	ClaudeAPIKey string `json:"claude_api_key,omitempty"`
	GeminiAPIKey string `json:"gemini_api_key,omitempty"`
	OpenAIAPIKey string `json:"openai_api_key,omitempty"`
	OllamaURL    string `json:"ollama_url,omitempty"`

	// Default model for different tasks
	DefaultModel  string `json:"default_model,omitempty"`
	AnalysisModel string `json:"analysis_model,omitempty"`
	CodingModel   string `json:"coding_model,omitempty"`
	ReviewModel   string `json:"review_model,omitempty"`

	// Provider-specific model IDs (empty = use provider default)
	ClaudeModel string `json:"claude_model,omitempty"`
	GeminiModel string `json:"gemini_model,omitempty"`
	OpenAIModel string `json:"openai_model,omitempty"`
	OllamaModel string `json:"ollama_model,omitempty"`

	// Usage mode and provider access types
	UsageMode    string `json:"usage_mode,omitempty"`    // "free","economy","balanced","power"
	ClaudeAccess string `json:"claude_access,omitempty"` // "subscription" or "api"
	GeminiAccess string `json:"gemini_access,omitempty"` // "free","subscription","api"
	OpenAIAccess string `json:"openai_access,omitempty"` // "subscription" or "api"
	CodexAccess  string `json:"codex_access,omitempty"`  // "subscription" or "api"
	OllamaAccess string `json:"ollama_access,omitempty"` // always "local"

	// UI preferences
	Language string `json:"language,omitempty"` // "zh" or "en"
	Theme    string `json:"theme,omitempty"`    // "dark" or "light"

	// Cost tracking
	MonthlyBudget float64 `json:"monthly_budget,omitempty"`
	TotalSpent    float64 `json:"total_spent,omitempty"`

	// Detected CLI tools (not persisted, populated at load time)
	CLIs []CLITool `json:"-"`

	// CustomProviders allows users to add private/custom model providers via
	// command-line adapters without changing makewand source code.
	CustomProviders []CustomProvider `json:"custom_providers,omitempty"`

	// envSourcedKeys tracks which API keys came from environment variables
	// so they are not persisted to disk.
	envSourcedKeys map[string]bool
}

// CLITool represents a detected subscription CLI tool.
type CLITool struct {
	Name    string // "claude", "gemini", "codex"
	BinPath string // absolute path
	Version string // version string
}

// CustomProvider describes one command-based external provider.
// PromptMode controls how the prompt is delivered:
//   - "stdin": write prompt to process stdin (recommended)
//   - "arg": append prompt as final argument
//   - empty: keep legacy "{{prompt}}" replacement / argv append behavior
type CustomProvider struct {
	Name       string   `json:"name"`
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	Access     string   `json:"access,omitempty"`      // "free","local","subscription","api"
	PromptMode string   `json:"prompt_mode,omitempty"` // "stdin","arg"; empty keeps legacy argv/{{prompt}} delivery
}

const (
	CustomPromptModeLegacy = "legacy"
	CustomPromptModeArg    = "arg"
	CustomPromptModeStdin  = "stdin"
)

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DefaultModel:  "claude",
		AnalysisModel: "gemini",
		CodingModel:   "claude",
		ReviewModel:   "gemini",
		Language:      "en",
		Theme:         "dark",
		MonthlyBudget: 20.0,
	}
}

// ConfigDir returns the path to the config directory.
// Priority:
//  1. MAKEWAND_CONFIG_DIR (if set)
//  2. ~/.config/makewand
func ConfigDir() (string, error) {
	dir := strings.TrimSpace(os.Getenv("MAKEWAND_CONFIG_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "makewand")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create config dir %s: %w", dir, err)
	}
	return dir, nil
}

// ConfigPath returns the path to the config file.
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config from disk, returning defaults if not found.
func Load() (*Config, error) {
	cfg := DefaultConfig()
	var loadErr error

	path, err := ConfigPath()
	if err == nil {
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			if err := json.Unmarshal(data, cfg); err != nil {
				// Keep defaults on parse failure, but continue with env/CLI discovery.
				cfg = DefaultConfig()
				loadErr = fmt.Errorf("parse config: %w", err)
			}
		} else if !os.IsNotExist(readErr) {
			loadErr = fmt.Errorf("read config: %w", readErr)
		}
	} else {
		loadErr = fmt.Errorf("resolve config path: %w", err)
	}

	// Override with environment variables (track which keys came from env)
	cfg.envSourcedKeys = make(map[string]bool)
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg.ClaudeAPIKey = key
		cfg.envSourcedKeys["claude"] = true
	}
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		cfg.GeminiAPIKey = key
		cfg.envSourcedKeys["gemini"] = true
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		cfg.OpenAIAPIKey = key
		cfg.envSourcedKeys["openai"] = true
	}

	// Auto-detect installed CLI tools
	cfg.CLIs = detectCLIs()

	// If CLI tools found, set access to subscription (unless explicitly overridden)
	for _, cli := range cfg.CLIs {
		switch cli.Name {
		case "claude":
			if cfg.ClaudeAccess == "" {
				cfg.ClaudeAccess = "subscription"
			}
		case "gemini":
			if cfg.GeminiAccess == "" {
				cfg.GeminiAccess = "subscription"
			}
		case "codex":
			if cfg.CodexAccess == "" {
				cfg.CodexAccess = "subscription"
			}
		}
	}

	return cfg, loadErr
}

// Save writes the config to disk, stripping env-sourced API keys.
func Save(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}

	// Create a copy that strips env-sourced keys
	toSave := *cfg
	if cfg.envSourcedKeys != nil {
		if cfg.envSourcedKeys["claude"] {
			toSave.ClaudeAPIKey = ""
		}
		if cfg.envSourcedKeys["gemini"] {
			toSave.GeminiAPIKey = ""
		}
		if cfg.envSourcedKeys["openai"] {
			toSave.OpenAIAPIKey = ""
		}
	}

	data, err := json.MarshalIndent(&toSave, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// HasAnyModel returns true if at least one model is configured (API key or CLI tool).
func (c *Config) HasAnyModel() bool {
	if c.ClaudeAPIKey != "" || c.GeminiAPIKey != "" || c.OpenAIAPIKey != "" || len(c.CLIs) > 0 {
		return true
	}
	for _, cp := range c.CustomProviders {
		if IsCustomProviderUsable(cp) {
			return true
		}
	}
	return false
}

// IsCustomProviderUsable returns true when the custom provider has a non-empty
// name and command, and the command can be executed on this machine.
func IsCustomProviderUsable(cp CustomProvider) bool {
	if strings.TrimSpace(cp.Name) == "" {
		return false
	}
	command := strings.TrimSpace(cp.Command)
	if command == "" {
		return false
	}

	// Explicit path (absolute/relative) must exist and be executable.
	if strings.ContainsAny(command, `/\`) {
		info, err := os.Stat(command)
		if err != nil || info.IsDir() {
			return false
		}
		return info.Mode()&0o111 != 0
	}

	// Bare executable name must resolve from PATH.
	_, err := exec.LookPath(command)
	return err == nil
}

// EffectiveCustomProviderPromptMode normalizes prompt delivery mode.
// Empty values keep legacy behavior for backward compatibility.
func EffectiveCustomProviderPromptMode(cp CustomProvider) string {
	switch strings.ToLower(strings.TrimSpace(cp.PromptMode)) {
	case CustomPromptModeStdin:
		return CustomPromptModeStdin
	case CustomPromptModeArg:
		return CustomPromptModeArg
	default:
		return CustomPromptModeLegacy
	}
}

// CustomProviderUsesShellAdapter returns true for commands that invoke a shell
// interpreter with an inline command string (for example sh -c, bash -lc, cmd /c).
func CustomProviderUsesShellAdapter(cp CustomProvider) bool {
	command := strings.ToLower(filepath.Base(strings.TrimSpace(cp.Command)))
	if command == "" || len(cp.Args) == 0 {
		return false
	}

	firstArg := strings.ToLower(strings.TrimSpace(cp.Args[0]))
	switch command {
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
		return firstArg == "-c" || firstArg == "-lc" || firstArg == "-ic"
	case "cmd", "cmd.exe":
		return firstArg == "/c"
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return firstArg == "-command" || firstArg == "-c"
	default:
		return false
	}
}

// HasCLI returns true if a specific CLI tool was detected.
func (c *Config) HasCLI(name string) bool {
	for _, cli := range c.CLIs {
		if cli.Name == name {
			return true
		}
	}
	return false
}

// GetCLI returns the CLI info for a given name, or nil if not found.
func (c *Config) GetCLI(name string) *CLITool {
	for i := range c.CLIs {
		if c.CLIs[i].Name == name {
			return &c.CLIs[i]
		}
	}
	return nil
}

// detectCLIs probes the system for installed subscription CLI tools.
func detectCLIs() []CLITool {
	type probe struct {
		name    string
		bin     string
		verArgs []string
	}
	probes := []probe{
		{"claude", "claude", []string{"--version"}},
		{"gemini", "gemini", []string{"--version"}},
		{"codex", "codex", []string{"--version"}},
	}

	var results []CLITool
	for _, p := range probes {
		binPath, err := exec.LookPath(p.bin)
		if err != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, binPath, p.verArgs...).Output()
		cancel()

		version := "detected"
		if err == nil && len(out) > 0 {
			version = strings.TrimSpace(strings.Split(string(out), "\n")[0])
		}

		results = append(results, CLITool{
			Name:    p.name,
			BinPath: binPath,
			Version: version,
		})
	}
	return results
}
