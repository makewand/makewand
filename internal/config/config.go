package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds the application configuration.
type Config struct {
	// Model API keys
	ClaudeAPIKey string `json:"claude_api_key,omitempty"`
	GeminiAPIKey string `json:"gemini_api_key,omitempty"`
	OpenAIAPIKey string `json:"openai_api_key,omitempty"`
	OllamaURL    string `json:"ollama_url,omitempty"`

	// Default model for different tasks
	DefaultModel   string `json:"default_model,omitempty"`
	AnalysisModel  string `json:"analysis_model,omitempty"`
	CodingModel    string `json:"coding_model,omitempty"`
	ReviewModel    string `json:"review_model,omitempty"`

	// UI preferences
	Language string `json:"language,omitempty"` // "zh" or "en"
	Theme    string `json:"theme,omitempty"`    // "dark" or "light"

	// Cost tracking
	MonthlyBudget float64 `json:"monthly_budget,omitempty"`
	TotalSpent    float64 `json:"total_spent,omitempty"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		OllamaURL:     "http://localhost:11434",
		DefaultModel:  "claude",
		AnalysisModel: "gemini",
		CodingModel:   "claude",
		ReviewModel:   "gemini",
		Language:      "en",
		Theme:         "dark",
		MonthlyBudget: 20.0,
	}
}

// ConfigDir returns the path to the config directory (~/.config/makewand/).
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "makewand")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
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

	path, err := ConfigPath()
	if err != nil {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return DefaultConfig(), err
	}

	// Override with environment variables
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg.ClaudeAPIKey = key
	}
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		cfg.GeminiAPIKey = key
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		cfg.OpenAIAPIKey = key
	}

	return cfg, nil
}

// Save writes the config to disk.
func Save(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// HasAnyModel returns true if at least one model is configured.
func (c *Config) HasAnyModel() bool {
	return c.ClaudeAPIKey != "" || c.GeminiAPIKey != "" || c.OpenAIAPIKey != "" || c.OllamaURL != ""
}
