package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/makewand/makewand/internal/config"
)

// NewRouter creates a Router from a config.Config (CLI adapter).
// Library consumers should use NewRouterFromConfig instead.
func NewRouter(cfg *config.Config) *Router {
	rc := RouterConfig{}

	// Load user routing overrides.
	if dir, err := config.ConfigDir(); err == nil {
		rc.ConfigDir = dir
	}

	rc.DefaultModel = cfg.DefaultModel
	rc.AnalysisModel = cfg.AnalysisModel
	rc.CodingModel = cfg.CodingModel
	rc.ReviewModel = cfg.ReviewModel
	rc.UsageMode = cfg.UsageMode

	rc.Providers = make(map[string]ProviderEntry)

	if config.HasRemoteBackend() {
		rc.Providers["remote"] = ProviderEntry{
			Provider: NewRemoteHTTP(config.RemoteBaseURL(), config.RemoteToken()),
			Access:   AccessAPI,
		}
		return NewRouterFromConfig(rc)
	}

	// agy (Antigravity CLI) takes the "gemini" slot when installed: personal
	// Gemini subscriptions no longer flow through `gemini -p` (metered API).
	hasAgy := false
	for _, cli := range cfg.CLIs {
		if cli.Name == "agy" {
			hasAgy = true
			break
		}
	}

	// Register CLI-based providers first (subscription — preferred)
	for _, cli := range cfg.CLIs {
		switch cli.Name {
		case "claude":
			rc.Providers["claude"] = ProviderEntry{Provider: NewClaudeCLI(cli.BinPath), Access: AccessSubscription}
		case "gemini":
			if !hasAgy {
				rc.Providers["gemini"] = ProviderEntry{Provider: NewGeminiCLI(cli.BinPath), Access: AccessSubscription}
			}
		case "agy":
			rc.Providers["gemini"] = ProviderEntry{Provider: NewAgyCLI(cli.BinPath), Access: AccessSubscription}
		case "codex":
			rc.Providers["codex"] = ProviderEntry{Provider: NewCodexCLI(cli.BinPath), Access: AccessSubscription}
		}
	}

	// Register API-based providers (only if CLI not already registered)
	if cfg.ClaudeAPIKey != "" {
		if _, exists := rc.Providers["claude"]; !exists {
			rc.Providers["claude"] = ProviderEntry{Provider: NewClaude(cfg.ClaudeAPIKey, cfg.ClaudeModel), Access: AccessAPI}
		}
	}
	if cfg.GeminiAPIKey != "" {
		if _, exists := rc.Providers["gemini"]; !exists {
			rc.Providers["gemini"] = ProviderEntry{Provider: NewGemini(cfg.GeminiAPIKey, cfg.GeminiModel), Access: AccessAPI}
		}
	}
	if cfg.OpenAIAPIKey != "" {
		if _, exists := rc.Providers["openai"]; !exists {
			rc.Providers["openai"] = ProviderEntry{Provider: NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIModel), Access: AccessAPI}
		}
	}

	// Register custom command providers.
	for _, cp := range cfg.CustomProviders {
		if !config.IsCustomProviderUsable(cp) {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(cp.Name))
		command := strings.TrimSpace(cp.Command)
		if name == "" || command == "" {
			continue
		}
		if _, exists := rc.Providers[name]; exists {
			continue
		}
		access := strings.TrimSpace(cp.Access)
		if access == "" {
			access = "subscription"
		}
		rc.Providers[name] = ProviderEntry{
			Provider: NewCommandCLI(name, command, cp.Args, config.EffectiveCustomProviderPromptMode(cp)),
			Access:   ParseAccessType(access, name),
		}
	}

	// Subscription-quota awareness: attach a snapshotter so routing can steer by
	// remaining headroom. The refresh loop's lifecycle lives here at the CLI
	// layer (context.Background for the process lifetime); NewRouterFromConfig —
	// used by tests and library embedders — stays quota-free unless they opt in.
	snap := NewDefaultQuotaSnapshotter(0)
	rc.Quota = snap

	r := NewRouterFromConfig(rc)

	// Populate the first snapshot and begin periodic refresh without blocking
	// startup (the Claude source makes a network call).
	go snap.Start(context.Background())

	// Apply explicit access type overrides from config (may override subscription defaults).
	if cfg.ClaudeAccess != "" {
		r.SetAccessType("claude", ParseAccessType(cfg.ClaudeAccess, "claude"))
	}
	if cfg.GeminiAccess != "" {
		r.SetAccessType("gemini", ParseAccessType(cfg.GeminiAccess, "gemini"))
	}
	if cfg.OpenAIAccess != "" {
		r.SetAccessType("openai", ParseAccessType(cfg.OpenAIAccess, "openai"))
	}
	if cfg.CodexAccess != "" {
		r.SetAccessType("codex", ParseAccessType(cfg.CodexAccess, "codex"))
	}

	// Register factories for dynamic resolution in mode-based routing.
	_ = RegisterProviderFactory("claude", func(modelID string) (Provider, error) {
		if cli := cfg.GetCLI("claude"); cli != nil {
			return NewClaudeCLI(cli.BinPath), nil
		}
		if cfg.ClaudeAPIKey != "" {
			return NewClaude(cfg.ClaudeAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("claude not configured")
	})
	_ = RegisterProviderFactory("codex", func(modelID string) (Provider, error) {
		if cli := cfg.GetCLI("codex"); cli != nil {
			return NewCodexCLI(cli.BinPath), nil
		}
		if cfg.OpenAIAPIKey != "" {
			return NewOpenAI(cfg.OpenAIAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("codex not configured")
	})
	_ = RegisterProviderFactory("gemini", func(modelID string) (Provider, error) {
		// Prefer agy (Antigravity) — the current transport for personal Gemini
		// subscriptions — over the metered `gemini -p` / API key paths.
		if cli := cfg.GetCLI("agy"); cli != nil {
			return NewAgyCLI(cli.BinPath), nil
		}
		if cli := cfg.GetCLI("gemini"); cli != nil {
			return NewGeminiCLI(cli.BinPath), nil
		}
		if cfg.GeminiAPIKey != "" {
			return NewGemini(cfg.GeminiAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("gemini not configured")
	})

	return r
}
