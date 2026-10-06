package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// Config holds the application configuration.
type Config struct {
	// APIPolicy controls direct cloud API use and paid API fallback. Keys alone
	// do not opt in; remote gateway policy belongs to that server.
	APIPolicy string `json:"api_policy,omitempty"`

	// Model API keys
	ClaudeAPIKey  string `json:"claude_api_key,omitempty"`
	GeminiAPIKey  string `json:"gemini_api_key,omitempty"`
	OpenAIAPIKey  string `json:"openai_api_key,omitempty"`
	ClaudeBaseURL string `json:"claude_base_url,omitempty"`
	GeminiBaseURL string `json:"gemini_base_url,omitempty"`
	OpenAIBaseURL string `json:"openai_base_url,omitempty"`
	OllamaURL     string `json:"ollama_url,omitempty"`

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
	UsageMode    string `json:"usage_mode,omitempty"`    // "fast","balanced","power"
	ClaudeAccess string `json:"claude_access,omitempty"` // "subscription" or "api"
	GeminiAccess string `json:"gemini_access,omitempty"` // "free","subscription","api"
	OpenAIAccess string `json:"openai_access,omitempty"` // "subscription" or "api"
	CodexAccess  string `json:"codex_access,omitempty"`  // "subscription" or "api"
	OllamaAccess string `json:"ollama_access,omitempty"` // always "local"

	// UI preferences
	Language     string `json:"language,omitempty"`      // "zh" or "en"
	Theme        string `json:"theme,omitempty"`         // "dark" or "light"
	ApprovalMode string `json:"approval_mode,omitempty"` // "manual", "safe", or "autopilot"

	// One-time acknowledgment that MAKEWAND_UNSAFE_HOST_EXEC=1 may run
	// AI-generated commands directly on the host. The version is bumped whenever
	// the risk statement changes, forcing re-acknowledgment; the hostname binds
	// the acknowledgment to this machine so a copied config does not carry it.
	UnsafeHostExecAckVersion int    `json:"unsafe_host_exec_ack_version,omitempty"`
	UnsafeHostExecAckAt      string `json:"unsafe_host_exec_ack_at,omitempty"` // RFC3339
	UnsafeHostExecAckHost    string `json:"unsafe_host_exec_ack_host,omitempty"`

	// Cost tracking. MonthlyBudget is measured against month-to-date pay-as-you-go
	// spend tracked in a persistent ledger (internal/tui MonthlyLedger) that
	// survives /clear, new sessions, and restarts, and rolls over each calendar
	// month. The session cost panel is separate and remains per-conversation.
	MonthlyBudget float64 `json:"monthly_budget,omitempty"`
	TotalSpent    float64 `json:"total_spent,omitempty"`

	// Detected CLI tools (not persisted, populated at load time)
	CLIs []CLITool `json:"-"`

	// CustomProviders allows users to add private/custom model providers via
	// command-line adapters without changing makewand source code.
	CustomProviders []CustomProvider `json:"custom_providers,omitempty"`

	// Provider controls are read by both engines but owned by the Python
	// frontend. Save preserves the latest disk values instead of this snapshot.
	EnabledProviders  map[string]bool `json:"-"`
	ActiveProviders   []string        `json:"-"`
	LocalModelEnabled *bool           `json:"-"`

	// envSourcedKeys tracks keys resolved from the environment or api_keys.json
	// so they are not copied into config.json.
	envSourcedKeys    map[string]bool
	externalAPIFields map[string]apiFieldSource
	// An invalid policy remains locked even if a caller ignores Load's error or
	// environment switches otherwise enable a provider.
	policyLoadErr error
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
	APIPolicySubscriptionOnly = "subscription_only"
	APIPolicyAllowPaid        = "allow_paid"

	CustomPromptModeLegacy = "legacy"
	CustomPromptModeArg    = "arg"
	CustomPromptModeStdin  = "stdin"

	ApprovalModeManual = "manual"
	ApprovalModeSafe   = "safe"
	ApprovalModeAuto   = "autopilot"

	UsageModeFast     = "fast"
	UsageModeBalanced = "balanced"
	UsageModePower    = "power"
	DefaultUsageMode  = UsageModeBalanced

	// UnsafeHostExecAckCurrentVersion is the version of the unsafe host
	// execution risk statement. Bump it whenever the statement (or the security
	// model behind it) changes materially; stored acknowledgments with an older
	// version become invalid and the user must re-acknowledge.
	UnsafeHostExecAckCurrentVersion = 1
)

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		APIPolicy:     APIPolicySubscriptionOnly,
		DefaultModel:  "claude",
		AnalysisModel: "gemini",
		CodingModel:   "claude",
		ReviewModel:   "gemini",
		UsageMode:     DefaultUsageMode,
		Language:      "en",
		Theme:         "dark",
		ApprovalMode:  ApprovalModeManual,
		MonthlyBudget: 20.0,
	}
}

// NormalizeAPIPolicy fails closed for absent and unknown values.
func NormalizeAPIPolicy(policy string) string {
	if strings.ToLower(strings.TrimSpace(policy)) == APIPolicyAllowPaid {
		return APIPolicyAllowPaid
	}
	return APIPolicySubscriptionOnly
}

// EffectiveAPIPolicy gives an explicit environment override precedence over
// configuration. Even a misspelled/empty override disables direct API spend.
func (c *Config) EffectiveAPIPolicy() string {
	if c != nil && c.policyLoadErr != nil {
		return APIPolicySubscriptionOnly
	}
	if policy, present := os.LookupEnv("MAKEWAND_API_POLICY"); present {
		return NormalizeAPIPolicy(policy)
	}
	if c == nil {
		return APIPolicySubscriptionOnly
	}
	return NormalizeAPIPolicy(c.APIPolicy)
}

func (c *Config) PaidAPIAllowed() bool { return c.EffectiveAPIPolicy() == APIPolicyAllowPaid }

// NormalizeUsageMode returns a supported usage mode, defaulting to balanced.
// Accepts canonical Go modes (fast, balanced, power) and Python aliases (standard, deep).
// This also migrates legacy configs where usage_mode was absent, empty, or invalid.
func NormalizeUsageMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case UsageModeFast:
		return UsageModeFast
	case UsageModeBalanced, "standard":
		return UsageModeBalanced
	case UsageModePower, "deep":
		return UsageModePower
	default:
		return DefaultUsageMode
	}
}

// NormalizeApprovalMode returns a supported approval mode, defaulting to manual.
func NormalizeApprovalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ApprovalModeSafe:
		return ApprovalModeSafe
	case ApprovalModeAuto:
		return ApprovalModeAuto
	default:
		return ApprovalModeManual
	}
}

// ParseApprovalMode strictly parses a user-supplied approval mode. Unlike
// NormalizeApprovalMode (which migrates legacy/unknown config values to manual)
// it rejects anything that is not exactly manual, safe, or autopilot, so CLI
// flag validation can surface typos instead of silently degrading to manual.
func ParseApprovalMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ApprovalModeManual:
		return ApprovalModeManual, true
	case ApprovalModeSafe:
		return ApprovalModeSafe, true
	case ApprovalModeAuto:
		return ApprovalModeAuto, true
	default:
		return "", false
	}
}

// ackHostname is a test seam for os.Hostname.
var ackHostname = os.Hostname

// UnsafeHostExecAckValid reports whether this config carries a valid one-time
// acknowledgment for MAKEWAND_UNSAFE_HOST_EXEC host execution: the recorded
// version must be at least the current risk-statement version and the recorded
// hostname must match this machine. Any error resolving the local hostname
// invalidates the acknowledgment (fail closed).
func (c *Config) UnsafeHostExecAckValid() bool {
	if c == nil || c.UnsafeHostExecAckVersion < UnsafeHostExecAckCurrentVersion {
		return false
	}
	if strings.TrimSpace(c.UnsafeHostExecAckHost) == "" {
		return false
	}
	host, err := ackHostname()
	if err != nil {
		return false
	}
	return c.UnsafeHostExecAckHost == host
}

// RecordUnsafeHostExecAck stamps the current acknowledgment version, time, and
// hostname onto the config. It returns an error when the local hostname cannot
// be resolved, because an acknowledgment that cannot be bound to this machine
// would validate on any machine the config is copied to.
func (c *Config) RecordUnsafeHostExecAck(now time.Time) error {
	host, err := ackHostname()
	if err != nil {
		return fmt.Errorf("resolve hostname for unsafe host exec acknowledgment: %w", err)
	}
	c.UnsafeHostExecAckVersion = UnsafeHostExecAckCurrentVersion
	c.UnsafeHostExecAckAt = now.UTC().Format(time.RFC3339)
	c.UnsafeHostExecAckHost = host
	return nil
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
	dir = filepath.Clean(dir)
	//nolint:gosec // G703: the path deliberately comes from the user's own MAKEWAND_CONFIG_DIR (or homedir); a local CLI honoring its user's config-dir choice is not path traversal.
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

// LoadOptions controls optional behavior of config loading.
type LoadOptions struct {
	// SkipCLIDetection disables probing the system for installed subscription
	// CLI tools (the `claude/gemini/codex/agy --version` execs in detectEnabledCLIs).
	// Untrusted-repository mode sets this: those probes inherit the process
	// working directory (the untrusted repo) where a CLI may load project
	// config, and untrusted mode only ever uses direct API providers anyway, so
	// probing local CLIs is both unnecessary and unsafe. The trusted default
	// (Load) leaves detection on when the execution policy is valid.
	SkipCLIDetection bool
}

// Load reads the config from disk, returning defaults if not found. It performs
// the full trusted-default load, including subscription CLI detection.
func Load() (*Config, error) {
	return LoadWithOptions(LoadOptions{})
}

// LoadWithOptions reads the config from disk like Load, honoring opts. See
// LoadOptions for the available knobs. Load() is LoadWithOptions(LoadOptions{}).
func LoadWithOptions(opts LoadOptions) (*Config, error) {
	cfg := DefaultConfig()

	path, err := ConfigPath()
	if err != nil {
		return invalidPolicyConfig(fmt.Errorf("resolve config path: %w", err))
	}
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := validateJSONObject(data); err != nil {
			return invalidPolicyConfig(fmt.Errorf("parse config: %w", err))
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			return invalidPolicyConfig(fmt.Errorf("parse config: %w", err))
		}
		if err := cfg.loadProviderControls(data); err != nil {
			return invalidPolicyConfig(fmt.Errorf("invalid provider controls: %w", err))
		}
	} else if !os.IsNotExist(readErr) {
		return invalidPolicyConfig(fmt.Errorf("read config: %w", readErr))
	}

	// Shared precedence: explicit environment > api_keys.json > config.json.
	loadErr := cfg.loadSharedAPIConfig(path)

	// Auto-detect installed CLI tools. Skipped in untrusted-repository mode so we
	// never exec a local CLI (its `--version` probe) inside an untrusted working
	// directory; only direct API providers are used in that mode.
	if !opts.SkipCLIDetection {
		cfg.CLIs = detectEnabledCLIs(cfg)
	}
	cfg.APIPolicy = NormalizeAPIPolicy(cfg.APIPolicy)
	cfg.UsageMode = NormalizeUsageMode(cfg.UsageMode)
	cfg.ApprovalMode = NormalizeApprovalMode(cfg.ApprovalMode)

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

// Save updates Go-owned fields while preserving fields used by the Python
// engine in the shared config file. Environment-sourced keys are never saved.
func Save(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("cannot save nil config")
	}
	if cfg.policyLoadErr != nil {
		return fmt.Errorf("cannot save invalid configuration: %w", cfg.policyLoadErr)
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}

	// Create a copy that strips env-sourced keys
	toSave := *cfg
	toSave.restoreExternalAPIFields()
	toSave.APIPolicy = NormalizeAPIPolicy(toSave.APIPolicy)
	toSave.UsageMode = NormalizeUsageMode(toSave.UsageMode)
	toSave.ApprovalMode = NormalizeApprovalMode(toSave.ApprovalMode)
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

	// Read the current disk version, not a snapshot taken at Load: another
	// frontend may have changed its own fields while this Go process was open.
	merged := make(map[string]json.RawMessage)
	if existing, err := os.ReadFile(path); err == nil {
		if err := validateJSONObject(existing); err != nil {
			return fmt.Errorf("preserve existing config: %w", err)
		}
		if err := json.Unmarshal(existing, &merged); err != nil {
			return fmt.Errorf("preserve existing config: %w", err)
		}
		if merged == nil {
			merged = make(map[string]json.RawMessage)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing config: %w", err)
	}

	// Remove ALL Go fields first, including omitted/cleared values and case
	// aliases accepted by encoding/json. Merely overlaying Marshal's output
	// would resurrect removed values, including old or environment-sourced keys.
	known := make(map[string]bool)
	typ := reflect.TypeFor[Config]()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		known[strings.ToLower(name)] = true
	}
	for key := range merged {
		if known[strings.ToLower(key)] {
			delete(merged, key)
		}
	}
	encoded, err := json.Marshal(&toSave)
	if err != nil {
		return err
	}
	var updates map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &updates); err != nil {
		return err
	}
	for key, value := range updates {
		merged[key] = value
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	if err := validateProviderControls(data); err != nil {
		return fmt.Errorf("cannot save invalid provider controls: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// HasAnyModel returns true if at least one model is configured (API key or CLI tool).
func (c *Config) HasAnyModel() bool {
	for _, cli := range c.CLIs {
		if c.IsProviderEnabled(cli.Name) {
			return true
		}
	}
	if c.PaidAPIAllowed() && (c.IsProviderEnabled("claude") && c.ClaudeAPIKey != "" || c.IsProviderEnabled("gemini") && c.GeminiAPIKey != "" || c.IsProviderEnabled("openai") && c.OpenAIAPIKey != "") {
		return true
	}
	for _, cp := range c.CustomProviders {
		if !c.IsProviderEnabled(cp.Name) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(cp.Access), "api") && !c.PaidAPIAllowed() {
			continue
		}
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
		// Windows does not expose POSIX execute bits. Apply its native
		// executable-extension lookup without executing a provider probe.
		if runtime.GOOS == "windows" {
			resolved, err := exec.LookPath(command)
			return err == nil && hasWindowsExecutableExtension(resolved)
		}
		return info.Mode()&0o111 != 0
	}

	// Bare executable name must resolve from PATH.
	resolved, err := exec.LookPath(command)
	if err != nil {
		return false
	}
	return runtime.GOOS != "windows" || hasWindowsExecutableExtension(resolved)
}

// LookPath also returns an existing ordinary file when its name already has
// an extension on Windows. Check PATHEXT separately before advertising it as
// a usable command, without executing a provider availability probe.
func hasWindowsExecutableExtension(path string) bool {
	ext := filepath.Ext(path)
	if ext == "" {
		return false
	}
	pathExt := os.Getenv("PATHEXT")
	if pathExt == "" {
		pathExt = ".COM;.EXE;.BAT;.CMD"
	}
	for _, allowed := range strings.Split(pathExt, ";") {
		if allowed == "" {
			continue
		}
		if !strings.HasPrefix(allowed, ".") {
			allowed = "." + allowed
		}
		if strings.EqualFold(ext, allowed) {
			return true
		}
	}
	return false
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
	if !c.IsProviderEnabled(name) {
		return false
	}
	for _, cli := range c.CLIs {
		if cli.Name == name {
			return true
		}
	}
	return false
}

// GetCLI returns the CLI info for a given name, or nil if not found.
func (c *Config) GetCLI(name string) *CLITool {
	if !c.IsProviderEnabled(name) {
		return nil
	}
	for i := range c.CLIs {
		if c.CLIs[i].Name == name {
			return &c.CLIs[i]
		}
	}
	return nil
}

// detectEnabledCLIs probes enabled subscription CLI tools, skipping disabled
// providers before PATH lookup or execution of their version commands.
func detectEnabledCLIs(cfg *Config) []CLITool {
	type probe struct {
		name    string
		bin     string
		verArgs []string
	}
	probes := []probe{
		{"claude", "claude", []string{"--version"}},
		{"gemini", "gemini", []string{"--version"}},
		{"codex", "codex", []string{"--version"}},
		// agy (Antigravity CLI) is the current transport for personal Gemini
		// subscriptions; when present it takes the "gemini" provider slot.
		{"agy", "agy", []string{"--version"}},
	}

	var results []CLITool
	for _, p := range probes {
		if !cfg.IsProviderEnabled(p.name) {
			continue
		}
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
