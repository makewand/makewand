package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/makewand/makewand/internal/config"
)

// NewRouter creates a Router from a config.Config (CLI adapter).
// Library consumers should use NewRouterFromConfig instead.
// It returns an error when the router cannot be constructed, e.g. when the
// user's routing.json overrides are invalid.
//
// NewRouter constructs a trusted-repository router (the existing default). Entry
// points that support untrusted-repository mode must use NewRouterWithTrust so
// the trust level is known at CONSTRUCTION — before the async quota refresh
// starts, which can exec local CLIs that are unsafe against an untrusted repo.
func NewRouter(cfg *config.Config) (*Router, error) {
	return NewRouterWithTrust(cfg, RepoTrustTrusted)
}

// NewRouterWithTrust creates a Router from a config.Config with an explicit
// repository trust level applied at construction. Setting RouterConfig.RepoTrust
// before NewRouterFromConfig ensures the router's trust is established before any
// background work (e.g. quota/health refresh) runs, so untrusted mode fails
// closed end-to-end rather than after a post-construction SetRepoTrust call.
func NewRouterWithTrust(cfg *config.Config, trust RepoTrust) (*Router, error) {
	rc := RouterConfig{}
	rc.RepoTrust = trust

	// Point the Router at the config dir so it loads routing.json into its own
	// per-instance tables. NewRouterFromConfig applies and validates them and
	// returns an error on failure — we no longer mirror overrides into the
	// package-level tables (that leaked one config's overrides into every
	// Router). Instance callers should use the Router's ContextBudgetForMode.
	//
	// Surface a ConfigDir failure instead of swallowing it: silently dropping the
	// config dir would skip routing.json (and stats persistence) with no signal.
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}
	rc.ConfigDir = dir

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

	// Register CLI-based providers first (subscription — preferred). When the
	// matching API key also exists, keep an `*-api` sibling in the pool so the
	// router can fall back to paid API usage after the subscription path fails.
	for _, cli := range cfg.CLIs {
		switch cli.Name {
		case "claude":
			if strings.EqualFold(cfg.ClaudeAccess, "api") && cfg.ClaudeAPIKey != "" {
				rc.Providers["claude"] = ProviderEntry{Provider: NewClaude(cfg.ClaudeAPIKey, cfg.ClaudeModel), Access: AccessAPI}
				continue
			}
			rc.Providers["claude"] = ProviderEntry{Provider: NewClaudeCLI(cli.BinPath), Access: AccessSubscription}
			if cfg.ClaudeAPIKey != "" {
				rc.Providers["claude-api"] = ProviderEntry{Provider: NewClaude(cfg.ClaudeAPIKey, cfg.ClaudeModel), Access: AccessAPI}
			}
		case "gemini":
			if !hasAgy {
				if strings.EqualFold(cfg.GeminiAccess, "api") && cfg.GeminiAPIKey != "" {
					rc.Providers["gemini"] = ProviderEntry{Provider: NewGemini(cfg.GeminiAPIKey, cfg.GeminiModel), Access: AccessAPI}
					continue
				}
				rc.Providers["gemini"] = ProviderEntry{Provider: NewGeminiCLI(cli.BinPath), Access: AccessSubscription}
				if cfg.GeminiAPIKey != "" {
					rc.Providers["gemini-api"] = ProviderEntry{Provider: NewGemini(cfg.GeminiAPIKey, cfg.GeminiModel), Access: AccessAPI}
				}
			}
		case "agy":
			if strings.EqualFold(cfg.GeminiAccess, "api") && cfg.GeminiAPIKey != "" {
				rc.Providers["gemini"] = ProviderEntry{Provider: NewGemini(cfg.GeminiAPIKey, cfg.GeminiModel), Access: AccessAPI}
				continue
			}
			rc.Providers["gemini"] = ProviderEntry{Provider: NewAgyCLI(cli.BinPath), Access: AccessSubscription}
			if cfg.GeminiAPIKey != "" {
				rc.Providers["gemini-api"] = ProviderEntry{Provider: NewGemini(cfg.GeminiAPIKey, cfg.GeminiModel), Access: AccessAPI}
			}
		case "codex":
			rc.Providers["codex"] = ProviderEntry{Provider: NewCodexCLI(cli.BinPath), Access: AccessSubscription}
		}
	}

	// Register API-based providers when no subscription CLI is present.
	// If a CLI is present, the `*-api` sibling above keeps API fallback available.
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
	// remaining headroom. NewRouterFromConfig — used by tests and library
	// embedders — stays quota-free unless they opt in.
	snap := NewDefaultQuotaSnapshotter(0)
	rc.Quota = snap

	r, err := NewRouterFromConfig(rc)
	if err != nil {
		return nil, err
	}

	// Populate the first snapshot once, off the startup path (the Claude source
	// makes a network call). This is a one-shot goroutine that terminates — no
	// long-lived ticker — so repeated NewRouter calls (e.g. eval/bench loops)
	// don't leak background refreshers. Long-running servers that want periodic
	// refresh call snap.Start(ctx) with a cancellable context of their own.
	go snap.Refresh(context.Background())

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

	// Register per-instance factories for dynamic resolution in mode-based
	// routing, so concurrent routers built from different configs never share
	// factory closures.
	_ = r.RegisterProviderFactory("claude", func(modelID string) (Provider, error) {
		if cli := cfg.GetCLI("claude"); cli != nil {
			return NewClaudeCLI(cli.BinPath), nil
		}
		if cfg.ClaudeAPIKey != "" {
			return NewClaude(cfg.ClaudeAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("claude not configured")
	})
	_ = r.RegisterProviderFactory("codex", func(modelID string) (Provider, error) {
		if cli := cfg.GetCLI("codex"); cli != nil {
			return NewCodexCLI(cli.BinPath), nil
		}
		if cfg.OpenAIAPIKey != "" {
			return NewOpenAI(cfg.OpenAIAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("codex not configured")
	})
	_ = r.RegisterProviderFactory("gemini", func(modelID string) (Provider, error) {
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

	return r, nil
}
