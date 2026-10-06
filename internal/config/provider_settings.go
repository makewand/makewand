package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var providerAliases = map[string]string{
	"anthropic": "claude", "openai": "codex", "gemini": "agy", "google": "agy",
	"ollama": "local", "xai": "grok", "meta": "muse", "dashscope": "qwen", "aliyun": "qwen",
	"zhipu": "glm", "moonshot": "kimi", "silicon": "siliconflow", "deepseek-coder": "deepseek", "deepseek-chat": "deepseek",
}

// NormalizeProviderName uses the Python engine's canonical provider identities.
// API siblings belong to the same enablement control as their CLI provider.
func NormalizeProviderName(name string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), "-api")
	if canonical, ok := providerAliases[name]; ok {
		return canonical
	}
	return name
}

func providerNames(name string) []string {
	canonical := NormalizeProviderName(name)
	names := []string{}
	for alias, target := range providerAliases {
		if target == canonical {
			names = append(names, alias)
		}
	}
	sort.Strings(names)
	return append(names, canonical) // canonical wins conflicting aliases
}

// IsProviderEnabled matches Python's defaults, legacy fields, canonical
// settings, per-provider environment switches, then the environment allowlist.
func (c *Config) IsProviderEnabled(name string) bool {
	if c != nil && c.policyLoadErr != nil {
		return false
	}
	canonical := NormalizeProviderName(name)
	enabled := canonical != "local"
	if c != nil {
		if c.ActiveProviders != nil {
			enabled = false
			for _, active := range c.ActiveProviders {
				if NormalizeProviderName(active) == canonical {
					enabled = true
					break
				}
			}
		}
		if canonical == "local" && c.LocalModelEnabled != nil {
			enabled = *c.LocalModelEnabled
		}
		settings := make([]string, 0, len(c.EnabledProviders))
		for candidate := range c.EnabledProviders {
			if NormalizeProviderName(candidate) == canonical {
				settings = append(settings, candidate)
			}
		}
		sort.Slice(settings, func(i, j int) bool {
			ci, cj := strings.ToLower(strings.TrimSpace(settings[i])) == canonical, strings.ToLower(strings.TrimSpace(settings[j])) == canonical
			if ci != cj {
				return !ci
			}
			return settings[i] < settings[j]
		})
		for _, candidate := range settings {
			enabled = c.EnabledProviders[candidate]
		}
	}
	truthy := func(value string) bool { return value == "1" || value == "true" || value == "yes" }
	for _, candidate := range providerNames(canonical) {
		if truthy(os.Getenv("MAKEWAND_DISABLE_" + strings.ToUpper(candidate))) {
			enabled = false
		}
		if truthy(os.Getenv("MAKEWAND_ENABLE_" + strings.ToUpper(candidate))) {
			enabled = true
		}
	}
	if whitelist := os.Getenv("MAKEWAND_ENABLE_PROVIDERS"); whitelist != "" {
		enabled = false
		for _, candidate := range strings.Split(whitelist, ",") {
			if NormalizeProviderName(candidate) == canonical {
				enabled = true
				break
			}
		}
	}
	return enabled
}

// validateProviderControls rejects ambiguous authorization values. Whole-field
// null retains the legacy "unset" meaning; entries themselves are not nullable.
func validateProviderControls(data []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	if raw := document["enabled_providers"]; len(raw) != 0 && strings.TrimSpace(string(raw)) != "null" {
		var values map[string]any
		if err := json.Unmarshal(raw, &values); err != nil {
			return errors.New("enabled_providers must be an object of booleans")
		}
		for _, value := range values {
			if _, ok := value.(bool); !ok {
				return errors.New("enabled_providers entries must be booleans")
			}
		}
	}
	if raw := document["active_providers"]; len(raw) != 0 && strings.TrimSpace(string(raw)) != "null" {
		var values []any
		if err := json.Unmarshal(raw, &values); err != nil {
			return errors.New("active_providers must be an array of strings")
		}
		for _, value := range values {
			if _, ok := value.(string); !ok {
				return errors.New("active_providers entries must be strings")
			}
		}
	}
	if raw := document["local_model_enabled"]; len(raw) != 0 && strings.TrimSpace(string(raw)) != "null" {
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("local_model_enabled must be a boolean")
		}
	}
	return nil
}

func (c *Config) loadProviderControls(data []byte) error {
	if err := validateProviderControls(data); err != nil {
		return err
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil {
		return nil
	}
	var values map[string]any
	if json.Unmarshal(document["enabled_providers"], &values) == nil {
		c.EnabledProviders = make(map[string]bool)
		for key, value := range values {
			if enabled, ok := value.(bool); ok {
				c.EnabledProviders[key] = enabled
			}
		}
	}
	var active []any
	if json.Unmarshal(document["active_providers"], &active) == nil && active != nil {
		c.ActiveProviders = []string{}
		for _, value := range active {
			if name, ok := value.(string); ok {
				c.ActiveProviders = append(c.ActiveProviders, name)
			}
		}
	}
	var local bool
	if json.Unmarshal(document["local_model_enabled"], &local) == nil && strings.TrimSpace(string(document["local_model_enabled"])) != "null" {
		c.LocalModelEnabled = &local
	}
	return nil
}

// apiEntry resolves aliases deterministically. Canonical names override aliases.
func apiEntry(document map[string]any, provider string) map[string]any {
	entry := make(map[string]any)
	for _, name := range providerNames(provider) {
		if fields, ok := document[name].(map[string]any); ok {
			for key, value := range fields {
				entry[key] = value
			}
		}
	}
	return entry
}

func stringField(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// loadSharedAPIConfig resolves supported native API providers without creating
// a provider or using the network. Values from external sources are not copied
// into config.json when a Go frontend saves UI preferences.
type apiFieldSource struct{ original, resolved string }

func (c *Config) loadSharedAPIConfig(configPath string) error {
	c.envSourcedKeys = make(map[string]bool)
	c.externalAPIFields = make(map[string]apiFieldSource)
	keys := make(map[string]any)
	var sourceErr error
	if configPath != "" {
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "api_keys.json")); err == nil {
			if err := validateJSONObject(data); err != nil {
				sourceErr = fmt.Errorf("parse api_keys.json: %w", err)
			} else if err := json.Unmarshal(data, &keys); err != nil {
				sourceErr = fmt.Errorf("parse api_keys.json: %w", err)
			}
		} else if !os.IsNotExist(err) {
			sourceErr = fmt.Errorf("read api_keys.json: %w", err)
		}
	}
	if sourceErr != nil {
		keys = make(map[string]any)
	}
	var document map[string]any
	if data, err := os.ReadFile(configPath); err == nil {
		_ = json.Unmarshal(data, &document)
	}
	nested, _ := document["api"].(map[string]any)
	entries := []struct {
		provider, prefix            string
		key, model, baseURL         *string
		keyEnvs, modelEnvs, urlEnvs []string
	}{
		{"claude", "claude", &c.ClaudeAPIKey, &c.ClaudeModel, &c.ClaudeBaseURL, []string{"ANTHROPIC_API_KEY"}, []string{"ANTHROPIC_MODEL"}, []string{"ANTHROPIC_BASE_URL"}},
		{"agy", "gemini", &c.GeminiAPIKey, &c.GeminiModel, &c.GeminiBaseURL, []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, []string{"GEMINI_MODEL"}, []string{"GEMINI_BASE_URL"}},
		{"codex", "openai", &c.OpenAIAPIKey, &c.OpenAIModel, &c.OpenAIBaseURL, []string{"OPENAI_API_KEY"}, []string{"OPENAI_MODEL"}, []string{"OPENAI_BASE_URL"}},
	}
	for _, entry := range entries {
		fallback := apiEntry(nested, entry.provider)
		file := apiEntry(keys, entry.provider)
		fields := []struct {
			name  string
			value *string
			envs  []string
		}{{"api_key", entry.key, entry.keyEnvs}, {"model", entry.model, entry.modelEnvs}, {"base_url", entry.baseURL, entry.urlEnvs}}
		for _, field := range fields {
			original := *field.value
			*field.value = strings.TrimSpace(*field.value)
			if *field.value == "" {
				*field.value = stringField(fallback, field.name)
			}
			if value := stringField(file, field.name); value != "" {
				*field.value = value
			}
			for _, env := range field.envs {
				if value := strings.TrimSpace(os.Getenv(env)); value != "" {
					*field.value = value
					break
				}
			}
			if *field.value != original {
				c.externalAPIFields[entry.prefix+"_"+field.name] = apiFieldSource{original: original, resolved: *field.value}
			}
			if field.name == "api_key" && *field.value != original {
				c.envSourcedKeys[entry.prefix] = true
			}
		}
	}
	if sourceErr != nil {
		return &CredentialSourceWarning{Err: sourceErr}
	}
	return nil
}

func (c *Config) restoreExternalAPIFields() {
	fields := map[string]*string{"claude_model": &c.ClaudeModel, "gemini_model": &c.GeminiModel, "openai_model": &c.OpenAIModel, "claude_base_url": &c.ClaudeBaseURL, "gemini_base_url": &c.GeminiBaseURL, "openai_base_url": &c.OpenAIBaseURL}
	for name, source := range c.externalAPIFields {
		if field := fields[name]; field != nil && *field == source.resolved {
			*field = source.original
		}
	}
}
