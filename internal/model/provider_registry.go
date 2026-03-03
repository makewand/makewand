package model

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/makewand/makewand/internal/config"
)

// ProviderResolver constructs a provider instance for the given model ID.
// Implementations can ignore modelID when the backend uses a fixed/default model.
type ProviderResolver func(cfg *config.Config, modelID string) (Provider, error)

var (
	resolverMu       sync.RWMutex
	providerResolver = map[string]ProviderResolver{}
)

func init() {
	registerBuiltinProviderResolvers()
}

func registerBuiltinProviderResolvers() {
	_ = RegisterProviderResolver("claude", func(cfg *config.Config, modelID string) (Provider, error) {
		if cli := cfg.GetCLI("claude"); cli != nil {
			return NewClaudeCLI(cli.BinPath), nil
		}
		if cfg.ClaudeAPIKey != "" {
			return NewClaude(cfg.ClaudeAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("claude not configured (no CLI or API key)")
	})
	_ = RegisterProviderResolver("codex", func(cfg *config.Config, modelID string) (Provider, error) {
		if cli := cfg.GetCLI("codex"); cli != nil {
			return NewCodexCLI(cli.BinPath), nil
		}
		if cfg.OpenAIAPIKey != "" {
			return NewOpenAI(cfg.OpenAIAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("codex not configured (no CLI or API key)")
	})
	_ = RegisterProviderResolver("openai", func(cfg *config.Config, modelID string) (Provider, error) {
		if cfg.OpenAIAPIKey != "" {
			return NewOpenAI(cfg.OpenAIAPIKey, modelID), nil
		}
		if cli := cfg.GetCLI("codex"); cli != nil {
			return NewCodexCLI(cli.BinPath), nil
		}
		return nil, fmt.Errorf("openai not configured")
	})
	_ = RegisterProviderResolver("gemini", func(cfg *config.Config, modelID string) (Provider, error) {
		if cli := cfg.GetCLI("gemini"); cli != nil {
			return NewGeminiCLI(cli.BinPath), nil
		}
		if cfg.GeminiAPIKey != "" {
			return NewGemini(cfg.GeminiAPIKey, modelID), nil
		}
		return nil, fmt.Errorf("gemini not configured (no CLI or API key)")
	})
	_ = RegisterProviderResolver("ollama", func(cfg *config.Config, modelID string) (Provider, error) {
		if cfg.OllamaURL == "" {
			return nil, fmt.Errorf("ollama URL not configured")
		}
		return NewOllama(cfg.OllamaURL, modelID), nil
	})
}

// RegisterProviderResolver registers or overrides a provider resolver.
func RegisterProviderResolver(name string, resolver ProviderResolver) error {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return fmt.Errorf("provider name is empty")
	}
	if resolver == nil {
		return fmt.Errorf("resolver is nil")
	}

	resolverMu.Lock()
	providerResolver[name] = resolver
	resolverMu.Unlock()
	return nil
}

func getProviderResolver(name string) (ProviderResolver, bool) {
	resolverMu.RLock()
	defer resolverMu.RUnlock()
	r, ok := providerResolver[name]
	return r, ok
}

func resolverNames() []string {
	resolverMu.RLock()
	defer resolverMu.RUnlock()
	names := make([]string, 0, len(providerResolver))
	for name := range providerResolver {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
