package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/internal/config"
)

// TaskType categorizes what kind of AI task is being performed.
type TaskType int

const (
	TaskAnalyze TaskType = iota // requirements analysis, planning
	TaskCode                    // code generation, implementation
	TaskReview                  // code review, bug finding
	TaskExplain                 // explanation for non-programmers
	TaskFix                     // error diagnosis and fixing
)

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"` // "user", "assistant", "system"
	Content string `json:"content"`
}

// StreamChunk is a piece of streaming response.
type StreamChunk struct {
	Content string
	Done    bool
	Error   error
}

// Usage tracks token usage and cost for a single request.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Cost         float64
	Model        string
	Provider     string
}

// RouteResult describes which provider was selected and whether a fallback occurred.
type RouteResult struct {
	Provider   Provider
	ModelID    string
	Requested  string
	Actual     string
	IsFallback bool
}

// Provider defines the interface all model providers must implement.
type Provider interface {
	Name() string
	IsAvailable() bool
	Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error)
	ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error)
}

// fallbackOrder defines the deterministic order for provider fallback.
var fallbackOrder = []string{"claude", "codex", "openai", "gemini", "ollama"}

// MaxTokensForTask returns the appropriate max tokens for the given task type.
func MaxTokensForTask(task TaskType) int {
	switch task {
	case TaskAnalyze, TaskExplain, TaskReview:
		return 4096
	case TaskCode, TaskFix:
		return 8192
	default:
		return 8192
	}
}

// providerKey is used to cache provider instances by (name, modelID).
type providerKey struct {
	name    string
	modelID string
}

// Router selects the best model provider for a given task.
type Router struct {
	cfg       *config.Config
	providers map[string]Provider

	// Provider cache for mode-based routing (provider+model → instance)
	providerCache map[providerKey]Provider
	providerMu    sync.Mutex

	// Access types for each provider
	accessTypes map[string]AccessType

	// Per-provider request count for load balancing
	usage *sessionUsage
	// Per-provider circuit breaker for transient/outage isolation.
	breaker *providerCircuitBreaker

	// Usage mode (guarded by modeMu)
	modeMu  sync.RWMutex
	mode    UsageMode
	modeSet bool // true = use mode-based routing

	// Cached available providers list
	mu            sync.Mutex
	cachedAvail   []string
	cachedAvailAt time.Time

	traceMu   sync.RWMutex
	traceSink TraceSink
}

const availCacheTTL = 10 * time.Second

// NewRouter creates a new model router with configured providers.
func NewRouter(cfg *config.Config) *Router {
	r := &Router{
		cfg:           cfg,
		providers:     make(map[string]Provider),
		providerCache: make(map[providerKey]Provider),
		accessTypes:   make(map[string]AccessType),
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(defaultCircuitFailureThreshold, defaultCircuitCooldown),
	}

	// Register CLI-based providers first (subscription — preferred)
	for _, cli := range cfg.CLIs {
		switch cli.Name {
		case "claude":
			r.providers["claude"] = NewClaudeCLI(cli.BinPath)
			r.accessTypes["claude"] = AccessSubscription
		case "gemini":
			r.providers["gemini"] = NewGeminiCLI(cli.BinPath)
			r.accessTypes["gemini"] = AccessSubscription
		case "codex":
			r.providers["codex"] = NewCodexCLI(cli.BinPath)
			r.accessTypes["codex"] = AccessSubscription
		}
	}

	// Register API-based providers (only if CLI not already registered for that provider)
	if cfg.ClaudeAPIKey != "" && r.providers["claude"] == nil {
		r.providers["claude"] = NewClaude(cfg.ClaudeAPIKey, cfg.ClaudeModel)
	}
	if cfg.GeminiAPIKey != "" && r.providers["gemini"] == nil {
		r.providers["gemini"] = NewGemini(cfg.GeminiAPIKey, cfg.GeminiModel)
	}
	if cfg.OpenAIAPIKey != "" && r.providers["openai"] == nil {
		r.providers["openai"] = NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIModel)
	}
	if cfg.OllamaURL != "" {
		r.providers["ollama"] = NewOllama(cfg.OllamaURL, cfg.OllamaModel)
	}

	// Register config-defined custom command providers.
	for _, cp := range cfg.CustomProviders {
		if !config.IsCustomProviderUsable(cp) {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(cp.Name))
		command := strings.TrimSpace(cp.Command)
		if name == "" || command == "" {
			continue
		}
		if _, exists := r.providers[name]; exists {
			// Built-ins win when names collide to avoid surprising overrides.
			continue
		}
		r.providers[name] = NewCommandCLI(name, command, cp.Args)

		access := strings.TrimSpace(cp.Access)
		if access == "" {
			access = "subscription"
		}
		r.accessTypes[name] = parseAccessType(access, name)
	}

	// Parse usage mode
	if cfg.UsageMode != "" {
		if mode, ok := ParseUsageMode(cfg.UsageMode); ok {
			r.mode = mode
			r.modeSet = true
		}
	}

	// Parse access types
	if cfg.ClaudeAccess != "" {
		r.accessTypes["claude"] = parseAccessType(cfg.ClaudeAccess, "claude")
	} else if _, ok := r.accessTypes["claude"]; !ok {
		r.accessTypes["claude"] = parseAccessType("", "claude")
	}
	if cfg.GeminiAccess != "" {
		r.accessTypes["gemini"] = parseAccessType(cfg.GeminiAccess, "gemini")
	} else if _, ok := r.accessTypes["gemini"]; !ok {
		r.accessTypes["gemini"] = parseAccessType("", "gemini")
	}
	if cfg.OpenAIAccess != "" {
		r.accessTypes["openai"] = parseAccessType(cfg.OpenAIAccess, "openai")
	} else if _, ok := r.accessTypes["openai"]; !ok {
		r.accessTypes["openai"] = parseAccessType("", "openai")
	}
	if cfg.CodexAccess != "" {
		r.accessTypes["codex"] = parseAccessType(cfg.CodexAccess, "codex")
	} else if _, ok := r.accessTypes["codex"]; !ok {
		r.accessTypes["codex"] = parseAccessType("", "codex")
	}
	if cfg.OllamaAccess != "" {
		r.accessTypes["ollama"] = parseAccessType(cfg.OllamaAccess, "ollama")
	} else if _, ok := r.accessTypes["ollama"]; !ok {
		r.accessTypes["ollama"] = parseAccessType("", "ollama")
	}

	return r
}

// SetMode sets the usage mode and enables mode-based routing.
func (r *Router) SetMode(m UsageMode) {
	r.modeMu.Lock()
	r.mode = m
	r.modeSet = true
	r.modeMu.Unlock()
}

// SetTraceSink enables or disables structured router trace events.
// Pass nil to disable tracing.
func (r *Router) SetTraceSink(sink TraceSink) {
	r.traceMu.Lock()
	r.traceSink = sink
	r.traceMu.Unlock()
}

// Mode returns the current usage mode.
func (r *Router) Mode() UsageMode {
	r.modeMu.RLock()
	defer r.modeMu.RUnlock()
	return r.mode
}

// ModeSet returns whether mode-based routing is enabled.
func (r *Router) ModeSet() bool {
	r.modeMu.RLock()
	defer r.modeMu.RUnlock()
	return r.modeSet
}

// effectiveMode returns the routing mode to use.
// When the mode has not been explicitly set it defaults to ModeBalanced,
// ensuring BuildProviderFor / RouteProvider / ChatWith never silently fall back
// to ModeFree (the zero value) when the user has not chosen a mode.
func (r *Router) effectiveMode() UsageMode {
	r.modeMu.RLock()
	defer r.modeMu.RUnlock()
	if r.modeSet {
		return r.mode
	}
	return ModeBalanced
}

func (r *Router) emitTrace(event TraceEvent) {
	r.traceMu.RLock()
	sink := r.traceSink
	r.traceMu.RUnlock()
	if sink == nil {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.Mode == "" {
		event.Mode = r.effectiveMode().String()
	}
	sink.Trace(event)
}

func (r *Router) isCircuitOpen(provider string) (bool, time.Duration) {
	if r.breaker == nil {
		return false, 0
	}
	return r.breaker.PeekOpen(provider)
}

func (r *Router) beforeProviderAttempt(provider string) (bool, time.Duration) {
	if r.breaker == nil {
		return true, 0
	}
	return r.breaker.BeforeAttempt(provider)
}

func (r *Router) recordProviderSuccess(provider string) {
	if r.breaker == nil {
		return
	}
	r.breaker.RecordSuccess(provider)
}

func (r *Router) recordProviderFailure(provider string) (bool, time.Time) {
	return r.recordProviderFailureForErr(provider, nil)
}

func (r *Router) recordProviderFailureForErr(provider string, callErr error) (bool, time.Time) {
	if r.breaker == nil {
		return false, time.Time{}
	}
	// Timeouts are usually transient provider saturation/outage signals.
	// Trip the breaker immediately so subsequent requests route elsewhere.
	if isTimeoutErr(callErr) {
		return r.breaker.RecordFailureWeighted(provider, defaultCircuitFailureThreshold)
	}
	return r.breaker.RecordFailure(provider)
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeoutErr interface {
		Timeout() bool
	}
	var te timeoutErr
	if errors.As(err, &te) && te.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "deadline exceeded")
}

// EmitTrace allows non-router components (for example TUI pipeline steps)
// to write into the same debug trace stream.
func (r *Router) EmitTrace(event TraceEvent) {
	r.emitTrace(event)
}

// Route selects the best provider for a given task type.
func (r *Router) Route(task TaskType) (RouteResult, error) {
	if r.ModeSet() {
		return r.routeByMode(task)
	}
	return r.routeLegacy(task)
}

// routeLegacy is the original routing logic (config-based model assignment).
func (r *Router) routeLegacy(task TaskType) (RouteResult, error) {
	var modelName string

	switch task {
	case TaskAnalyze, TaskExplain:
		modelName = r.cfg.AnalysisModel
	case TaskCode, TaskFix:
		modelName = r.cfg.CodingModel
	case TaskReview:
		modelName = r.cfg.ReviewModel
	default:
		modelName = r.cfg.DefaultModel
	}

	r.emitTrace(TraceEvent{
		Event:     "route_legacy_start",
		Task:      taskTypeName(task),
		Requested: modelName,
	})

	// Try the preferred model
	if p, ok := r.providers[modelName]; ok && p.IsAvailable() {
		if blocked, remaining := r.isCircuitOpen(modelName); blocked {
			r.emitTrace(TraceEvent{
				Event:     "route_candidate_skipped",
				Task:      taskTypeName(task),
				Requested: modelName,
				Selected:  modelName,
				Detail:    circuitOpenDetail(modelName, remaining),
			})
		} else {
			r.emitTrace(TraceEvent{
				Event:     "route_selected",
				Task:      taskTypeName(task),
				Requested: modelName,
				Selected:  modelName,
			})
			return RouteResult{
				Provider:  p,
				Requested: modelName,
				Actual:    modelName,
			}, nil
		}
	}

	// Deterministic fallback: try providers in defined order
	for _, name := range fallbackOrder {
		if name == modelName {
			continue
		}
		if p, ok := r.providers[name]; ok && p.IsAvailable() {
			if blocked, remaining := r.isCircuitOpen(name); blocked {
				r.emitTrace(TraceEvent{
					Event:      "route_candidate_skipped",
					Task:       taskTypeName(task),
					Requested:  modelName,
					Selected:   name,
					IsFallback: true,
					Detail:     circuitOpenDetail(name, remaining),
				})
				continue
			}
			r.emitTrace(TraceEvent{
				Event:      "route_selected",
				Task:       taskTypeName(task),
				Requested:  modelName,
				Selected:   name,
				IsFallback: true,
			})
			return RouteResult{
				Provider:   p,
				Requested:  modelName,
				Actual:     name,
				IsFallback: true,
			}, nil
		}
	}

	r.emitTrace(TraceEvent{
		Event:     "route_failed",
		Task:      taskTypeName(task),
		Requested: modelName,
		Error:     "no AI model available",
	})

	return RouteResult{}, fmt.Errorf("no AI model available; configure one with 'makewand setup'")
}

// routeByMode selects a provider using the strategy table and access-type sorting.
func (r *Router) routeByMode(task TaskType) (RouteResult, error) {
	entry, err := r.modeEntry(task)
	if err != nil {
		return RouteResult{}, err
	}

	phase := taskToBuildPhase(task)
	candidates := r.modeCandidates(entry, nil, phase)
	requested := ""
	if len(entry.Providers) > 0 {
		requested = entry.Providers[0]
	}

	r.emitTrace(TraceEvent{
		Event:      "route_mode_candidates",
		Task:       taskTypeName(task),
		Phase:      buildPhaseName(phase),
		Requested:  requested,
		Candidates: toTraceCandidates(candidates),
	})
	if len(candidates) == 0 {
		msg := fmt.Sprintf("no AI model available for mode %q; configure one with 'makewand setup'", r.effectiveMode())
		if r.effectiveMode() == ModeFree {
			msg = "free mode requires at least one free/local provider (e.g. Gemini free API or Ollama local model)"
		}
		r.emitTrace(TraceEvent{
			Event:     "route_failed",
			Task:      taskTypeName(task),
			Phase:     buildPhaseName(phase),
			Requested: requested,
			Error:     msg,
		})
		return RouteResult{}, fmt.Errorf("%s", msg)
	}

	// Try each candidate
	for _, c := range candidates {
		if blocked, remaining := r.isCircuitOpen(c.name); blocked {
			r.emitTrace(TraceEvent{
				Event:      "route_candidate_skipped",
				Task:       taskTypeName(task),
				Phase:      buildPhaseName(phase),
				Requested:  requested,
				Selected:   c.name,
				ModelID:    c.modelID,
				IsFallback: c.name != requested,
				Detail:     circuitOpenDetail(c.name, remaining),
			})
			continue
		}

		p, err := r.resolveProvider(c.name, c.modelID)
		if err != nil {
			r.emitTrace(TraceEvent{
				Event:     "route_candidate_skipped",
				Task:      taskTypeName(task),
				Phase:     buildPhaseName(phase),
				Requested: requested,
				Selected:  c.name,
				ModelID:   c.modelID,
				Error:     err.Error(),
			})
			continue
		}
		if !p.IsAvailable() {
			r.emitTrace(TraceEvent{
				Event:     "route_candidate_skipped",
				Task:      taskTypeName(task),
				Phase:     buildPhaseName(phase),
				Requested: requested,
				Selected:  c.name,
				ModelID:   c.modelID,
				Detail:    "provider unavailable",
			})
			continue
		}

		r.emitTrace(TraceEvent{
			Event:      "route_selected",
			Task:       taskTypeName(task),
			Phase:      buildPhaseName(phase),
			Requested:  requested,
			Selected:   c.name,
			ModelID:    c.modelID,
			IsFallback: c.name != requested,
		})
		return RouteResult{
			Provider:   p,
			ModelID:    c.modelID,
			Requested:  requested,
			Actual:     c.name,
			IsFallback: c.name != requested,
		}, nil
	}

	r.emitTrace(TraceEvent{
		Event:     "route_failed",
		Task:      taskTypeName(task),
		Phase:     buildPhaseName(phase),
		Requested: requested,
		Error:     "no AI model available for mode",
	})

	return RouteResult{}, fmt.Errorf("no AI model available for mode %q; configure one with 'makewand setup'", r.effectiveMode())
}

func (r *Router) modeEntry(task TaskType) (strategyEntry, error) {
	taskStrategies, ok := strategyTable[r.effectiveMode()]
	if !ok {
		return strategyEntry{}, fmt.Errorf("unknown usage mode: %d", r.effectiveMode())
	}

	entry, ok := taskStrategies[task]
	if !ok {
		entry = taskStrategies[TaskCode]
	}
	return entry, nil
}

func (r *Router) registeredProviderNames() []string {
	r.providerMu.Lock()
	defer r.providerMu.Unlock()

	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func expandProviderPreference(preferred []string, registered []string, excluded map[string]bool) []string {
	out := make([]string, 0, len(preferred)+len(registered))
	seen := make(map[string]bool, len(preferred)+len(registered))
	registeredSet := make(map[string]struct{}, len(registered))
	for _, name := range registered {
		registeredSet[name] = struct{}{}
	}

	for _, name := range preferred {
		if name == "" || seen[name] {
			continue
		}
		if excluded != nil && excluded[name] {
			continue
		}
		if _, ok := registeredSet[name]; !ok {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range registered {
		if seen[name] {
			continue
		}
		if excluded != nil && excluded[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func (r *Router) modeCandidates(entry strategyEntry, excluded map[string]bool, phase BuildPhase) []candidate {
	var candidates []candidate
	orderedProviders := expandProviderPreference(entry.Providers, r.registeredProviderNames(), excluded)
	staticOrder := make(map[string]int, len(entry.Providers))
	for i, name := range entry.Providers {
		staticOrder[name] = i
	}

	for i, provName := range orderedProviders {
		modelID := ""
		if models, ok := modelTable[provName]; ok {
			modelID = models[entry.Tier]
		}

		access := r.accessTypes[provName]

		// Free mode (strict): only free/local providers are allowed.
		if r.effectiveMode() == ModeFree && access != AccessFree && access != AccessLocal {
			continue
		}

		// Prior bias encodes static table preference:
		// position 0 (primary) → bias 2.0, position 1 → 1.0, position 2+ → 0.0.
		// This seeds the Beta distribution so observed quality can gradually override
		// the static order rather than starting from a blank slate.
		priorBias := 0.0
		if pos, ok := staticOrder[provName]; ok {
			priorBias = math.Max(0.0, float64(2-pos))
		}

		candidates = append(candidates, candidate{
			name:          provName,
			modelID:       modelID,
			access:        access,
			order:         i,
			useCount:      r.usage.Count(provName),
			failureRate:   r.usage.FailureRate(provName),
			requests:      r.usage.Count(provName) + r.usage.FailureCount(provName),
			thompsonScore: r.usage.ThompsonSample(phase, provName, priorBias),
		})
	}

	sortCandidates(candidates)
	return candidates
}

// resolveProvider returns a provider instance for the given (name, modelID) pair.
// It caches instances so the same (provider, model) combination reuses the same instance.
func (r *Router) resolveProvider(providerName, modelID string) (Provider, error) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	key := providerKey{name: providerName, modelID: modelID}

	r.providerMu.Lock()
	defer r.providerMu.Unlock()

	if p, ok := r.providerCache[key]; ok {
		return p, nil
	}

	// If a CLI provider is already registered for this provider name, use it
	// (CLI providers ignore modelID — they use subscription defaults)
	if existing, ok := r.providers[providerName]; ok {
		if _, isCLI := existing.(*CLIProvider); isCLI {
			r.providerCache[key] = existing
			return existing, nil
		}
	}

	resolver, ok := getProviderResolver(providerName)
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}
	p, err := resolver(r.cfg, modelID)
	if err != nil {
		return nil, err
	}

	r.providerCache[key] = p
	return p, nil
}

// Get returns a specific provider by name.
func (r *Router) Get(name string) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("model provider %q not configured", name)
	}
	if !p.IsAvailable() {
		return nil, fmt.Errorf("model provider %q is not available", name)
	}
	return p, nil
}

// RegisterProvider injects a provider instance at runtime.
// This is useful for private/custom providers without editing router internals.
func (r *Router) RegisterProvider(name string, provider Provider, access AccessType) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("provider name is empty")
	}
	if provider == nil {
		return fmt.Errorf("provider is nil")
	}

	r.providerMu.Lock()
	r.providers[name] = provider
	for k := range r.providerCache {
		if k.name == name {
			delete(r.providerCache, k)
		}
	}
	r.providerCache[providerKey{name: name, modelID: ""}] = provider
	r.providerMu.Unlock()

	r.mu.Lock()
	r.accessTypes[name] = access
	r.cachedAvailAt = time.Time{}
	r.mu.Unlock()
	return nil
}

// Available returns all available provider names, filtered by the effective mode.
// In ModeFree, only free/local providers are returned to match strict free routing.
// Results are cached to avoid repeated health checks (e.g. Ollama) on every render cycle.
func (r *Router) Available() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.cachedAvailAt) < availCacheTTL {
		return r.cachedAvail
	}

	mode := r.effectiveMode()
	var names []string
	for name, p := range r.providers {
		if p.IsAvailable() {
			if mode == ModeFree && r.accessTypes[name] != AccessFree && r.accessTypes[name] != AccessLocal {
				continue
			}
			if blocked, _ := r.isCircuitOpen(name); blocked {
				continue
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)

	r.cachedAvail = names
	r.cachedAvailAt = time.Now()
	return names
}

// IsSubscription returns true if the named provider uses subscription access.
func (r *Router) IsSubscription(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accessTypes[name] == AccessSubscription
}

// Chat sends a message using the best provider for the given task type.
// If the primary provider fails, it tries the next available provider.
func (r *Router) Chat(ctx context.Context, task TaskType, messages []Message, system string) (string, Usage, RouteResult, error) {
	result, err := r.Route(task)
	if err != nil {
		return "", Usage{}, RouteResult{}, err
	}
	maxTokens := MaxTokensForTask(task)
	firstErr := error(nil)

	if allow, remaining := r.beforeProviderAttempt(result.Actual); !allow {
		firstErr = fmt.Errorf("%s", circuitOpenDetail(result.Actual, remaining))
		r.emitTrace(TraceEvent{
			Event:      "chat_fallback_skipped",
			Task:       taskTypeName(task),
			Requested:  result.Requested,
			Selected:   result.Actual,
			ModelID:    result.ModelID,
			IsFallback: result.IsFallback,
			Detail:     firstErr.Error(),
		})
	} else {
		primaryStart := time.Now()
		content, usage, chatErr := result.Provider.Chat(ctx, messages, system, maxTokens)
		if chatErr == nil {
			r.usage.Increment(result.Actual)
			r.recordProviderSuccess(result.Actual)
			r.emitTrace(TraceEvent{
				Event:      "chat_attempt_success",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   result.Actual,
				ModelID:    result.ModelID,
				IsFallback: result.IsFallback,
				DurationMS: time.Since(primaryStart).Milliseconds(),
			})
			return content, usage, result, nil
		}
		r.emitTrace(TraceEvent{
			Event:      "chat_attempt_error",
			Task:       taskTypeName(task),
			Requested:  result.Requested,
			Selected:   result.Actual,
			ModelID:    result.ModelID,
			IsFallback: result.IsFallback,
			DurationMS: time.Since(primaryStart).Milliseconds(),
			Error:      chatErr.Error(),
		})

		r.usage.RecordFailure(result.Actual)
		if opened, until := r.recordProviderFailureForErr(result.Actual, chatErr); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Task:     taskTypeName(task),
				Selected: result.Actual,
				Detail:   circuitOpenDetail(result.Actual, time.Until(until)),
			})
		}
		firstErr = chatErr
	}

	if r.ModeSet() {
		entry, modeErr := r.modeEntry(task)
		if modeErr == nil {
			excluded := map[string]bool{result.Actual: true}
			for _, c := range r.modeCandidates(entry, excluded, taskToBuildPhase(task)) {
				if blocked, remaining := r.isCircuitOpen(c.name); blocked {
					r.emitTrace(TraceEvent{
						Event:      "chat_fallback_skipped",
						Task:       taskTypeName(task),
						Requested:  result.Requested,
						Selected:   c.name,
						ModelID:    c.modelID,
						IsFallback: true,
						Detail:     circuitOpenDetail(c.name, remaining),
					})
					continue
				}
				p, resolveErr := r.resolveProvider(c.name, c.modelID)
				if resolveErr != nil {
					r.emitTrace(TraceEvent{
						Event:     "chat_fallback_skipped",
						Task:      taskTypeName(task),
						Requested: result.Requested,
						Selected:  c.name,
						ModelID:   c.modelID,
						Error:     resolveErr.Error(),
					})
					continue
				}
				if !p.IsAvailable() {
					r.emitTrace(TraceEvent{
						Event:     "chat_fallback_skipped",
						Task:      taskTypeName(task),
						Requested: result.Requested,
						Selected:  c.name,
						ModelID:   c.modelID,
						Detail:    "provider unavailable",
					})
					continue
				}
				if allow, remaining := r.beforeProviderAttempt(c.name); !allow {
					r.emitTrace(TraceEvent{
						Event:      "chat_fallback_skipped",
						Task:       taskTypeName(task),
						Requested:  result.Requested,
						Selected:   c.name,
						ModelID:    c.modelID,
						IsFallback: true,
						Detail:     circuitOpenDetail(c.name, remaining),
					})
					continue
				}
				start := time.Now()
				content, usage, retryErr := p.Chat(ctx, messages, system, maxTokens)
				if retryErr == nil {
					r.usage.Increment(c.name)
					r.recordProviderSuccess(c.name)
					r.emitTrace(TraceEvent{
						Event:      "chat_attempt_success",
						Task:       taskTypeName(task),
						Requested:  result.Requested,
						Selected:   c.name,
						ModelID:    c.modelID,
						IsFallback: true,
						DurationMS: time.Since(start).Milliseconds(),
					})
					return content, usage, RouteResult{
						Provider:   p,
						ModelID:    c.modelID,
						Requested:  result.Requested,
						Actual:     c.name,
						IsFallback: true,
					}, nil
				}
				r.emitTrace(TraceEvent{
					Event:      "chat_attempt_error",
					Task:       taskTypeName(task),
					Requested:  result.Requested,
					Selected:   c.name,
					ModelID:    c.modelID,
					IsFallback: true,
					DurationMS: time.Since(start).Milliseconds(),
					Error:      retryErr.Error(),
				})
				r.usage.RecordFailure(c.name)
				if opened, until := r.recordProviderFailureForErr(c.name, retryErr); opened {
					r.emitTrace(TraceEvent{
						Event:    "circuit_opened",
						Task:     taskTypeName(task),
						Selected: c.name,
						ModelID:  c.modelID,
						Detail:   circuitOpenDetail(c.name, time.Until(until)),
					})
				}
				if firstErr == nil {
					firstErr = retryErr
				}
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("all providers skipped by circuit breaker")
			}
			return "", Usage{}, result, firstErr
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("route mode resolution failed")
		}
		return "", Usage{}, result, firstErr
	}

	for _, name := range fallbackOrder {
		if name == result.Actual {
			continue
		}
		if blocked, remaining := r.isCircuitOpen(name); blocked {
			r.emitTrace(TraceEvent{
				Event:      "chat_fallback_skipped",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   name,
				IsFallback: true,
				Detail:     circuitOpenDetail(name, remaining),
			})
			continue
		}
		p, ok := r.providers[name]
		if !ok || !p.IsAvailable() {
			continue
		}
		if allow, remaining := r.beforeProviderAttempt(name); !allow {
			r.emitTrace(TraceEvent{
				Event:      "chat_fallback_skipped",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   name,
				IsFallback: true,
				Detail:     circuitOpenDetail(name, remaining),
			})
			continue
		}
		start := time.Now()
		content, usage, retryErr := p.Chat(ctx, messages, system, maxTokens)
		if retryErr == nil {
			r.usage.Increment(name)
			r.recordProviderSuccess(name)
			r.emitTrace(TraceEvent{
				Event:      "chat_attempt_success",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   name,
				IsFallback: true,
				DurationMS: time.Since(start).Milliseconds(),
			})
			return content, usage, RouteResult{
				Provider:   p,
				Requested:  result.Requested,
				Actual:     name,
				IsFallback: true,
			}, nil
		}
		r.emitTrace(TraceEvent{
			Event:      "chat_attempt_error",
			Task:       taskTypeName(task),
			Requested:  result.Requested,
			Selected:   name,
			IsFallback: true,
			DurationMS: time.Since(start).Milliseconds(),
			Error:      retryErr.Error(),
		})
		r.usage.RecordFailure(name)
		if opened, until := r.recordProviderFailureForErr(name, retryErr); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Task:     taskTypeName(task),
				Selected: name,
				Detail:   circuitOpenDetail(name, time.Until(until)),
			})
		}
		if firstErr == nil {
			firstErr = retryErr
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no provider attempt was possible")
	}
	r.emitTrace(TraceEvent{
		Event:     "chat_failed_all",
		Task:      taskTypeName(task),
		Requested: result.Requested,
		Selected:  result.Actual,
		Error:     firstErr.Error(),
	})
	return "", Usage{}, result, firstErr
}

// BuildProviderFor returns the primary provider name for a build phase under the current mode.
func (r *Router) BuildProviderFor(phase BuildPhase) string {
	modeStrategies, ok := buildStrategyTable[r.effectiveMode()]
	if !ok {
		return ""
	}
	bs, ok := modeStrategies[phase]
	if !ok {
		return ""
	}
	return bs.Primary
}

func (r *Router) buildStrategyForPhase(phase BuildPhase) (BuildStrategy, error) {
	modeStrategies, ok := buildStrategyTable[r.effectiveMode()]
	if !ok {
		return BuildStrategy{}, fmt.Errorf("unknown usage mode: %d", r.effectiveMode())
	}
	bs, ok := modeStrategies[phase]
	if !ok {
		return BuildStrategy{}, fmt.Errorf("unknown build phase: %d", phase)
	}
	return bs, nil
}

func (r *Router) buildPhaseCandidates(phase BuildPhase, excluded map[string]bool) (BuildStrategy, []candidate, error) {
	bs, err := r.buildStrategyForPhase(phase)
	if err != nil {
		return BuildStrategy{}, nil, err
	}

	preferred := append([]string{bs.Primary}, bs.Fallbacks...)
	orderedProviders := expandProviderPreference(preferred, r.registeredProviderNames(), excluded)
	staticOrder := make(map[string]int, len(preferred))
	for i, name := range preferred {
		if _, exists := staticOrder[name]; !exists {
			staticOrder[name] = i
		}
	}

	mode := r.effectiveMode()
	candidates := make([]candidate, 0, len(orderedProviders))
	for i, provName := range orderedProviders {
		access := r.accessTypes[provName]
		// Free mode (strict): only free/local providers are allowed.
		if mode == ModeFree && access != AccessFree && access != AccessLocal {
			continue
		}

		modelID := ""
		if models, ok := modelTable[provName]; ok {
			modelID = models[bs.Tier]
		}

		priorBias := 0.0
		if pos, ok := staticOrder[provName]; ok {
			priorBias = math.Max(0.0, float64(2-pos))
		}

		candidates = append(candidates, candidate{
			name:          provName,
			modelID:       modelID,
			access:        access,
			order:         i,
			useCount:      r.usage.Count(provName),
			failureRate:   r.usage.FailureRate(provName),
			requests:      r.usage.Count(provName) + r.usage.FailureCount(provName),
			thompsonScore: r.usage.ThompsonSample(phase, provName, priorBias),
		})
	}

	sortCandidates(candidates)
	return bs, candidates, nil
}

func (r *Router) isBuildProviderAvailable(name, modelID string) bool {
	r.providerMu.Lock()
	defer r.providerMu.Unlock()

	if p, ok := r.providerCache[providerKey{name: name, modelID: modelID}]; ok {
		return p.IsAvailable()
	}
	if p, ok := r.providers[name]; ok {
		return p.IsAvailable()
	}
	return false
}

// BuildProviderForAdaptive returns the best available provider for a build phase,
// using Thompson Sampling to adaptively re-order the candidates from buildStrategyTable.
// Configured providers that appear in the phase's (primary + fallbacks) list are
// scored with ThompsonSample; the highest-scoring available provider wins.
// Falls back to BuildProviderFor when no candidates are available.
func (r *Router) BuildProviderForAdaptive(phase BuildPhase) string {
	bs, candidates, err := r.buildPhaseCandidates(phase, nil)
	if err != nil {
		return r.BuildProviderFor(phase)
	}

	if len(candidates) == 0 {
		r.emitTrace(TraceEvent{
			Event:     "build_adaptive_no_candidates",
			Phase:     buildPhaseName(phase),
			Requested: bs.Primary,
		})
		return bs.Primary
	}

	for _, c := range candidates {
		if blocked, _ := r.isCircuitOpen(c.name); blocked {
			continue
		}
		if !r.isBuildProviderAvailable(c.name, c.modelID) {
			continue
		}

		r.emitTrace(TraceEvent{
			Event:      "build_adaptive_selected",
			Phase:      buildPhaseName(phase),
			Requested:  bs.Primary,
			Selected:   c.name,
			ModelID:    c.modelID,
			IsFallback: c.name != bs.Primary,
			Candidates: toTraceCandidates(candidates),
		})
		return c.name
	}

	r.emitTrace(TraceEvent{
		Event:     "build_adaptive_no_candidates",
		Phase:     buildPhaseName(phase),
		Requested: bs.Primary,
		Detail:    "all candidates unavailable",
	})
	return bs.Primary
}

// LoadStats loads cross-session routing quality statistics from configDir.
func (r *Router) LoadStats(configDir string) error {
	return r.usage.Load(configDir)
}

// SaveStats persists the current session's routing quality statistics to configDir.
func (r *Router) SaveStats(configDir string) error {
	return r.usage.Save(configDir)
}

// RouteProvider resolves a specific provider by name for a build phase.
// If the named provider is unavailable, it falls back through the phase's Fallbacks list.
// Providers in the exclude list are skipped (used to enforce cross-model constraints).
func (r *Router) RouteProvider(name string, phase BuildPhase, exclude ...string) (RouteResult, error) {
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}
	bs, candidates, err := r.buildPhaseCandidates(phase, excluded)
	if err != nil {
		return RouteResult{}, err
	}

	r.emitTrace(TraceEvent{
		Event:      "build_route_start",
		Phase:      buildPhaseName(phase),
		Requested:  name,
		Detail:     "exclude=" + strings.Join(exclude, ","),
		Candidates: toTraceCandidates(candidates),
	})

	// Try the requested provider first (unless excluded)
	if !excluded[name] {
		if r.effectiveMode() == ModeFree && r.accessTypes[name] != AccessFree && r.accessTypes[name] != AccessLocal {
			r.emitTrace(TraceEvent{
				Event:     "build_route_candidate_skipped",
				Phase:     buildPhaseName(phase),
				Requested: name,
				Selected:  name,
				Detail:    "non-free provider blocked in free mode",
			})
		} else if blocked, remaining := r.isCircuitOpen(name); blocked {
			r.emitTrace(TraceEvent{
				Event:     "build_route_candidate_skipped",
				Phase:     buildPhaseName(phase),
				Requested: name,
				Selected:  name,
				Detail:    circuitOpenDetail(name, remaining),
			})
		} else {
			if p, modelID, resolveErr := r.tryBuildProvider(name, bs.Tier); resolveErr == nil && p.IsAvailable() {
				r.emitTrace(TraceEvent{
					Event:     "build_route_selected",
					Phase:     buildPhaseName(phase),
					Requested: name,
					Selected:  name,
					ModelID:   modelID,
				})
				return RouteResult{
					Provider:  p,
					ModelID:   modelID,
					Requested: name,
					Actual:    name,
				}, nil
			} else if resolveErr != nil {
				r.emitTrace(TraceEvent{
					Event:     "build_route_candidate_skipped",
					Phase:     buildPhaseName(phase),
					Requested: name,
					Selected:  name,
					Error:     resolveErr.Error(),
				})
			} else {
				r.emitTrace(TraceEvent{
					Event:     "build_route_candidate_skipped",
					Phase:     buildPhaseName(phase),
					Requested: name,
					Selected:  name,
					Detail:    "provider unavailable",
				})
			}
		}
	}

	// Fallback through adaptive candidate ranking.
	for _, c := range candidates {
		fb := c.name
		if fb == name || excluded[fb] {
			continue
		}
		if blocked, remaining := r.isCircuitOpen(fb); blocked {
			r.emitTrace(TraceEvent{
				Event:      "build_route_candidate_skipped",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				IsFallback: true,
				Detail:     circuitOpenDetail(fb, remaining),
			})
			continue
		}
		p, modelID, resolveErr := r.tryBuildProvider(fb, bs.Tier)
		if resolveErr != nil {
			r.emitTrace(TraceEvent{
				Event:      "build_route_candidate_skipped",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				IsFallback: true,
				Error:      resolveErr.Error(),
			})
			continue
		}
		if !p.IsAvailable() {
			r.emitTrace(TraceEvent{
				Event:      "build_route_candidate_skipped",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				IsFallback: true,
				Detail:     "provider unavailable",
			})
			continue
		}
		if p.IsAvailable() {
			r.emitTrace(TraceEvent{
				Event:      "build_route_selected",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				ModelID:    modelID,
				IsFallback: true,
			})
			return RouteResult{
				Provider:   p,
				ModelID:    modelID,
				Requested:  name,
				Actual:     fb,
				IsFallback: true,
			}, nil
		}
	}

	r.emitTrace(TraceEvent{
		Event:     "build_route_failed",
		Phase:     buildPhaseName(phase),
		Requested: name,
		Error:     "no provider available for build phase",
	})

	return RouteResult{}, fmt.Errorf("no provider available for build phase (requested %s)", name)
}

// tryBuildProvider resolves a provider instance for the given name and tier.
func (r *Router) tryBuildProvider(name string, tier ModelTier) (Provider, string, error) {
	models, ok := modelTable[name]
	if !ok {
		// Dynamically registered providers may not have a modelTable entry.
		p, err := r.resolveProvider(name, "")
		if err != nil {
			return nil, "", fmt.Errorf("no model table entry for %s and dynamic resolution failed: %w", name, err)
		}
		return p, "", nil
	}
	modelID := models[tier]
	p, err := r.resolveProvider(name, modelID)
	if err != nil {
		return nil, "", err
	}
	return p, modelID, nil
}

// maxTokensForPhase returns the max output tokens for a build phase.
func maxTokensForPhase(phase BuildPhase) int {
	switch phase {
	case PhaseCode:
		return 16384 // full project generation needs more than TaskCode's 8192
	case PhaseFix:
		return 8192
	case PhasePlan, PhaseReview:
		return 4096
	default:
		return 8192
	}
}

func buildPhaseAttemptTimeout(phase BuildPhase) time.Duration {
	switch phase {
	case PhasePlan:
		return 90 * time.Second
	case PhaseCode:
		return 150 * time.Second
	case PhaseReview:
		return 45 * time.Second
	case PhaseFix:
		return 60 * time.Second
	default:
		return 90 * time.Second
	}
}

func withProviderAttemptTimeout(ctx context.Context, phase BuildPhase) (context.Context, context.CancelFunc) {
	maxDur := buildPhaseAttemptTimeout(phase)
	if maxDur <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) <= maxDur {
			return ctx, func() {}
		}
	}
	return context.WithTimeout(ctx, maxDur)
}

// ChatWith sends a message to a specific provider for a build phase.
// It resolves the provider by name, falls back if unavailable, and tracks usage.
// Providers in the exclude list are never used (enforces cross-model constraints).
func (r *Router) ChatWith(ctx context.Context, name string, phase BuildPhase, messages []Message, system string, exclude ...string) (string, Usage, RouteResult, error) {
	result, err := r.RouteProvider(name, phase, exclude...)
	if err != nil {
		return "", Usage{}, RouteResult{}, err
	}

	maxTokens := maxTokensForPhase(phase)
	firstErr := error(nil)

	if allow, remaining := r.beforeProviderAttempt(result.Actual); !allow {
		firstErr = fmt.Errorf("%s", circuitOpenDetail(result.Actual, remaining))
		r.emitTrace(TraceEvent{
			Event:      "build_chat_fallback_skipped",
			Phase:      buildPhaseName(phase),
			Requested:  name,
			Selected:   result.Actual,
			ModelID:    result.ModelID,
			IsFallback: result.IsFallback,
			Detail:     firstErr.Error(),
		})
	} else {
		primaryStart := time.Now()
		content, usage, chatErr := result.Provider.Chat(ctx, messages, system, maxTokens)
		if chatErr == nil {
			r.usage.Increment(result.Actual)
			r.recordProviderSuccess(result.Actual)
			r.emitTrace(TraceEvent{
				Event:      "build_chat_attempt_success",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   result.Actual,
				ModelID:    result.ModelID,
				IsFallback: result.IsFallback,
				DurationMS: time.Since(primaryStart).Milliseconds(),
			})
			return content, usage, result, nil
		}
		r.emitTrace(TraceEvent{
			Event:      "build_chat_attempt_error",
			Phase:      buildPhaseName(phase),
			Requested:  name,
			Selected:   result.Actual,
			ModelID:    result.ModelID,
			IsFallback: result.IsFallback,
			DurationMS: time.Since(primaryStart).Milliseconds(),
			Error:      chatErr.Error(),
		})

		r.usage.RecordFailure(result.Actual)
		if opened, until := r.recordProviderFailureForErr(result.Actual, chatErr); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Phase:    buildPhaseName(phase),
				Selected: result.Actual,
				ModelID:  result.ModelID,
				Detail:   circuitOpenDetail(result.Actual, time.Until(until)),
			})
		}
		firstErr = chatErr
	}

	// Primary failed — try remaining fallbacks, respecting exclude list
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}
	excluded[result.Actual] = true

	bs, candidates, candErr := r.buildPhaseCandidates(phase, excluded)
	if candErr != nil {
		if firstErr == nil {
			firstErr = candErr
		}
		r.emitTrace(TraceEvent{
			Event:     "build_chat_failed_all",
			Phase:     buildPhaseName(phase),
			Requested: name,
			Selected:  result.Actual,
			Error:     firstErr.Error(),
		})
		return "", Usage{}, result, firstErr
	}

	for _, c := range candidates {
		fb := c.name
		if fb == result.Actual || excluded[fb] {
			continue
		}
		if blocked, remaining := r.isCircuitOpen(fb); blocked {
			r.emitTrace(TraceEvent{
				Event:      "build_chat_fallback_skipped",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				IsFallback: true,
				Detail:     circuitOpenDetail(fb, remaining),
			})
			continue
		}
		p, modelID, resolveErr := r.tryBuildProvider(fb, bs.Tier)
		if resolveErr != nil {
			r.emitTrace(TraceEvent{
				Event:     "build_chat_fallback_skipped",
				Phase:     buildPhaseName(phase),
				Requested: name,
				Selected:  fb,
				Error:     resolveErr.Error(),
			})
			continue
		}
		if !p.IsAvailable() {
			r.emitTrace(TraceEvent{
				Event:     "build_chat_fallback_skipped",
				Phase:     buildPhaseName(phase),
				Requested: name,
				Selected:  fb,
				Detail:    "provider unavailable",
			})
			continue
		}
		if allow, remaining := r.beforeProviderAttempt(fb); !allow {
			r.emitTrace(TraceEvent{
				Event:      "build_chat_fallback_skipped",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				IsFallback: true,
				Detail:     circuitOpenDetail(fb, remaining),
			})
			continue
		}
		start := time.Now()
		attemptCtx, attemptCancel := withProviderAttemptTimeout(ctx, phase)
		content, usage, retryErr := p.Chat(attemptCtx, messages, system, maxTokens)
		attemptCancel()
		if retryErr == nil {
			r.usage.Increment(fb)
			r.recordProviderSuccess(fb)
			r.emitTrace(TraceEvent{
				Event:      "build_chat_attempt_success",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				ModelID:    modelID,
				IsFallback: true,
				DurationMS: time.Since(start).Milliseconds(),
			})
			return content, usage, RouteResult{
				Provider:   p,
				ModelID:    modelID,
				Requested:  name,
				Actual:     fb,
				IsFallback: true,
			}, nil
		}
		r.emitTrace(TraceEvent{
			Event:      "build_chat_attempt_error",
			Phase:      buildPhaseName(phase),
			Requested:  name,
			Selected:   fb,
			IsFallback: true,
			DurationMS: time.Since(start).Milliseconds(),
			Error:      retryErr.Error(),
		})
		r.usage.RecordFailure(fb)
		if opened, until := r.recordProviderFailureForErr(fb, retryErr); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Phase:    buildPhaseName(phase),
				Selected: fb,
				Detail:   circuitOpenDetail(fb, time.Until(until)),
			})
		}
		if firstErr == nil {
			firstErr = retryErr
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no provider attempt was possible")
	}

	r.emitTrace(TraceEvent{
		Event:     "build_chat_failed_all",
		Phase:     buildPhaseName(phase),
		Requested: name,
		Selected:  result.Actual,
		Error:     firstErr.Error(),
	})

	return "", Usage{}, result, firstErr
}

// ChatStream sends a message and streams the response.
func (r *Router) ChatStream(ctx context.Context, task TaskType, messages []Message, system string) (<-chan StreamChunk, RouteResult, error) {
	result, err := r.Route(task)
	if err != nil {
		return nil, RouteResult{}, err
	}
	maxTokens := MaxTokensForTask(task)
	firstErr := error(nil)

	if allow, remaining := r.beforeProviderAttempt(result.Actual); !allow {
		firstErr = fmt.Errorf("%s", circuitOpenDetail(result.Actual, remaining))
		r.emitTrace(TraceEvent{
			Event:      "chat_stream_fallback_skipped",
			Task:       taskTypeName(task),
			Requested:  result.Requested,
			Selected:   result.Actual,
			ModelID:    result.ModelID,
			IsFallback: result.IsFallback,
			Detail:     firstErr.Error(),
		})
	} else {
		primaryStart := time.Now()
		ch, streamErr := result.Provider.ChatStream(ctx, messages, system, maxTokens)
		if streamErr == nil {
			r.usage.Increment(result.Actual)
			r.recordProviderSuccess(result.Actual)
			r.emitTrace(TraceEvent{
				Event:      "chat_stream_start_success",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   result.Actual,
				ModelID:    result.ModelID,
				IsFallback: result.IsFallback,
				DurationMS: time.Since(primaryStart).Milliseconds(),
			})
			return ch, result, nil
		}
		r.emitTrace(TraceEvent{
			Event:      "chat_stream_start_error",
			Task:       taskTypeName(task),
			Requested:  result.Requested,
			Selected:   result.Actual,
			ModelID:    result.ModelID,
			IsFallback: result.IsFallback,
			DurationMS: time.Since(primaryStart).Milliseconds(),
			Error:      streamErr.Error(),
		})
		r.usage.RecordFailure(result.Actual)
		if opened, until := r.recordProviderFailureForErr(result.Actual, streamErr); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Task:     taskTypeName(task),
				Selected: result.Actual,
				Detail:   circuitOpenDetail(result.Actual, time.Until(until)),
			})
		}
		firstErr = streamErr
	}

	if r.ModeSet() {
		entry, modeErr := r.modeEntry(task)
		if modeErr == nil {
			excluded := map[string]bool{result.Actual: true}
			for _, c := range r.modeCandidates(entry, excluded, taskToBuildPhase(task)) {
				if blocked, remaining := r.isCircuitOpen(c.name); blocked {
					r.emitTrace(TraceEvent{
						Event:      "chat_stream_fallback_skipped",
						Task:       taskTypeName(task),
						Requested:  result.Requested,
						Selected:   c.name,
						ModelID:    c.modelID,
						IsFallback: true,
						Detail:     circuitOpenDetail(c.name, remaining),
					})
					continue
				}
				p, resolveErr := r.resolveProvider(c.name, c.modelID)
				if resolveErr != nil {
					r.emitTrace(TraceEvent{
						Event:     "chat_stream_fallback_skipped",
						Task:      taskTypeName(task),
						Requested: result.Requested,
						Selected:  c.name,
						ModelID:   c.modelID,
						Error:     resolveErr.Error(),
					})
					continue
				}
				if !p.IsAvailable() {
					r.emitTrace(TraceEvent{
						Event:     "chat_stream_fallback_skipped",
						Task:      taskTypeName(task),
						Requested: result.Requested,
						Selected:  c.name,
						ModelID:   c.modelID,
						Detail:    "provider unavailable",
					})
					continue
				}
				if allow, remaining := r.beforeProviderAttempt(c.name); !allow {
					r.emitTrace(TraceEvent{
						Event:      "chat_stream_fallback_skipped",
						Task:       taskTypeName(task),
						Requested:  result.Requested,
						Selected:   c.name,
						ModelID:    c.modelID,
						IsFallback: true,
						Detail:     circuitOpenDetail(c.name, remaining),
					})
					continue
				}
				start := time.Now()
				ch, retryErr := p.ChatStream(ctx, messages, system, maxTokens)
				if retryErr == nil {
					r.usage.Increment(c.name)
					r.recordProviderSuccess(c.name)
					r.emitTrace(TraceEvent{
						Event:      "chat_stream_start_success",
						Task:       taskTypeName(task),
						Requested:  result.Requested,
						Selected:   c.name,
						ModelID:    c.modelID,
						IsFallback: true,
						DurationMS: time.Since(start).Milliseconds(),
					})
					return ch, RouteResult{
						Provider:   p,
						ModelID:    c.modelID,
						Requested:  result.Requested,
						Actual:     c.name,
						IsFallback: true,
					}, nil
				}
				r.emitTrace(TraceEvent{
					Event:      "chat_stream_start_error",
					Task:       taskTypeName(task),
					Requested:  result.Requested,
					Selected:   c.name,
					ModelID:    c.modelID,
					IsFallback: true,
					DurationMS: time.Since(start).Milliseconds(),
					Error:      retryErr.Error(),
				})
				r.usage.RecordFailure(c.name)
				if opened, until := r.recordProviderFailureForErr(c.name, retryErr); opened {
					r.emitTrace(TraceEvent{
						Event:    "circuit_opened",
						Task:     taskTypeName(task),
						Selected: c.name,
						ModelID:  c.modelID,
						Detail:   circuitOpenDetail(c.name, time.Until(until)),
					})
				}
				if firstErr == nil {
					firstErr = retryErr
				}
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("all providers skipped by circuit breaker")
			}
			return nil, result, firstErr
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("route mode resolution failed")
		}
		return nil, result, firstErr
	}

	for _, name := range fallbackOrder {
		if name == result.Actual {
			continue
		}
		if blocked, remaining := r.isCircuitOpen(name); blocked {
			r.emitTrace(TraceEvent{
				Event:      "chat_stream_fallback_skipped",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   name,
				IsFallback: true,
				Detail:     circuitOpenDetail(name, remaining),
			})
			continue
		}
		p, ok := r.providers[name]
		if !ok || !p.IsAvailable() {
			continue
		}
		if allow, remaining := r.beforeProviderAttempt(name); !allow {
			r.emitTrace(TraceEvent{
				Event:      "chat_stream_fallback_skipped",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   name,
				IsFallback: true,
				Detail:     circuitOpenDetail(name, remaining),
			})
			continue
		}
		start := time.Now()
		ch, retryErr := p.ChatStream(ctx, messages, system, maxTokens)
		if retryErr == nil {
			r.usage.Increment(name)
			r.recordProviderSuccess(name)
			r.emitTrace(TraceEvent{
				Event:      "chat_stream_start_success",
				Task:       taskTypeName(task),
				Requested:  result.Requested,
				Selected:   name,
				IsFallback: true,
				DurationMS: time.Since(start).Milliseconds(),
			})
			return ch, RouteResult{
				Provider:   p,
				Requested:  result.Requested,
				Actual:     name,
				IsFallback: true,
			}, nil
		}
		r.emitTrace(TraceEvent{
			Event:      "chat_stream_start_error",
			Task:       taskTypeName(task),
			Requested:  result.Requested,
			Selected:   name,
			IsFallback: true,
			DurationMS: time.Since(start).Milliseconds(),
			Error:      retryErr.Error(),
		})
		r.usage.RecordFailure(name)
		if opened, until := r.recordProviderFailureForErr(name, retryErr); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Task:     taskTypeName(task),
				Selected: name,
				Detail:   circuitOpenDetail(name, time.Until(until)),
			})
		}
		if firstErr == nil {
			firstErr = retryErr
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no provider attempt was possible")
	}

	r.emitTrace(TraceEvent{
		Event:     "chat_stream_failed_all",
		Task:      taskTypeName(task),
		Requested: result.Requested,
		Selected:  result.Actual,
		Error:     firstErr.Error(),
	})

	return nil, result, firstErr
}

// --- Power Mode Ensemble ---

// EnsembleResult holds one provider's response in a parallel ensemble run.
type EnsembleResult struct {
	Provider string
	ModelID  string
	Content  string
	Usage    Usage
}

// Ensemble runs the generator providers for a Power-mode phase in parallel.
// Excluded providers are skipped (e.g. code provider must not review its own output).
// Returns all successful results; the caller selects the winner.
func (r *Router) Ensemble(ctx context.Context, phase BuildPhase, messages []Message, system string, exclude ...string) []EnsembleResult {
	pe, ok := powerEnsembleTable[phase]
	if !ok {
		return nil
	}

	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}

	// Collect available generator slots
	type slot struct {
		name    string
		modelID string
		p       Provider
	}
	var slots []slot
	for _, name := range pe.Generators {
		if excluded[name] {
			continue
		}
		if blocked, remaining := r.isCircuitOpen(name); blocked {
			r.emitTrace(TraceEvent{
				Event:    "ensemble_generator_skipped",
				Phase:    buildPhaseName(phase),
				Selected: name,
				Detail:   circuitOpenDetail(name, remaining),
			})
			continue
		}
		p, modelID, err := r.tryBuildProvider(name, TierPremium)
		if err != nil || !p.IsAvailable() {
			continue
		}
		slots = append(slots, slot{name, modelID, p})
	}
	if len(slots) == 0 {
		r.emitTrace(TraceEvent{
			Event:  "ensemble_no_generators",
			Phase:  buildPhaseName(phase),
			Detail: "no available generator provider",
		})
		return nil
	}
	r.emitTrace(TraceEvent{
		Event:  "ensemble_start",
		Phase:  buildPhaseName(phase),
		Detail: "judge=" + pe.Judge,
	})

	maxTokens := maxTokensForPhase(phase)
	results := make([]EnsembleResult, len(slots))
	var wg sync.WaitGroup
	for i, s := range slots {
		wg.Add(1)
		go func(idx int, sl slot) {
			defer wg.Done()
			if allow, remaining := r.beforeProviderAttempt(sl.name); !allow {
				r.emitTrace(TraceEvent{
					Event:    "ensemble_generator_skipped",
					Phase:    buildPhaseName(phase),
					Selected: sl.name,
					ModelID:  sl.modelID,
					Detail:   circuitOpenDetail(sl.name, remaining),
				})
				return
			}
			start := time.Now()
			attemptCtx, attemptCancel := withProviderAttemptTimeout(ctx, phase)
			content, usage, err := sl.p.Chat(attemptCtx, messages, system, maxTokens)
			attemptCancel()
			if err != nil {
				r.usage.RecordFailure(sl.name)
				if opened, until := r.recordProviderFailureForErr(sl.name, err); opened {
					r.emitTrace(TraceEvent{
						Event:    "circuit_opened",
						Phase:    buildPhaseName(phase),
						Selected: sl.name,
						ModelID:  sl.modelID,
						Detail:   circuitOpenDetail(sl.name, time.Until(until)),
					})
				}
				r.emitTrace(TraceEvent{
					Event:      "ensemble_generator_error",
					Phase:      buildPhaseName(phase),
					Selected:   sl.name,
					ModelID:    sl.modelID,
					DurationMS: time.Since(start).Milliseconds(),
					Error:      err.Error(),
				})
				return
			}
			r.usage.Increment(sl.name)
			r.recordProviderSuccess(sl.name)
			r.emitTrace(TraceEvent{
				Event:      "ensemble_generator_success",
				Phase:      buildPhaseName(phase),
				Selected:   sl.name,
				ModelID:    sl.modelID,
				DurationMS: time.Since(start).Milliseconds(),
			})
			results[idx] = EnsembleResult{sl.name, sl.modelID, content, usage}
		}(i, s)
	}
	wg.Wait()

	var out []EnsembleResult
	for _, res := range results {
		if res.Content != "" {
			out = append(out, res)
		}
	}
	r.emitTrace(TraceEvent{
		Event:  "ensemble_complete",
		Phase:  buildPhaseName(phase),
		Detail: fmt.Sprintf("success=%d/%d", len(out), len(slots)),
	})
	return out
}

// judgeSelect asks the designated judge provider to pick the best result from an ensemble.
// The judge is asked to declare the winner on the first line as "WINNER: N", making
// attribution reliable and independent of the content. Records quality outcomes:
// the winning generator gets a success signal. Falls back to first result on error.
func (r *Router) judgeSelect(ctx context.Context, phase BuildPhase, results []EnsembleResult) EnsembleResult {
	if len(results) == 1 {
		// Single result — treat it as a success (no competition needed).
		r.usage.RecordQualityOutcome(phase, results[0].Provider, true)
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_single_result",
			Phase:    buildPhaseName(phase),
			Selected: results[0].Provider,
		})
		return results[0]
	}

	pe, ok := powerEnsembleTable[phase]
	if !ok {
		r.emitTrace(TraceEvent{
			Event:  "judge_skipped_missing_config",
			Phase:  buildPhaseName(phase),
			Detail: "power ensemble config missing",
		})
		return results[0]
	}

	judgeP, judgeModelID, err := r.tryBuildProvider(pe.Judge, TierPremium)
	if err != nil || !judgeP.IsAvailable() {
		reason := "judge unavailable"
		if err != nil {
			reason = err.Error()
		}
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_unavailable",
			Phase:    buildPhaseName(phase),
			Selected: pe.Judge,
			Error:    reason,
		})
		return results[0]
	}
	if blocked, remaining := r.isCircuitOpen(pe.Judge); blocked {
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_unavailable",
			Phase:    buildPhaseName(phase),
			Selected: pe.Judge,
			Detail:   circuitOpenDetail(pe.Judge, remaining),
		})
		return results[0]
	}
	if allow, remaining := r.beforeProviderAttempt(pe.Judge); !allow {
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_unavailable",
			Phase:    buildPhaseName(phase),
			Selected: pe.Judge,
			Detail:   circuitOpenDetail(pe.Judge, remaining),
		})
		return results[0]
	}

	var prompt strings.Builder
	for i, res := range results {
		prompt.WriteString(fmt.Sprintf("=== Option %d ===\n%s\n\n", i+1, res.Content))
	}
	// Ask judge to declare which option won on the very first line so attribution
	// is unambiguous regardless of the content (which might mention provider names).
	prompt.WriteString(fmt.Sprintf(
		"First line must be exactly \"WINNER: <N>\" where N is 1–%d.\nThen output the complete chosen option, completely unchanged:",
		len(results),
	))

	judgeMessages := []Message{{Role: "user", Content: prompt.String()}}
	judgeStart := time.Now()
	attemptCtx, attemptCancel := withProviderAttemptTimeout(ctx, phase)
	content, usage, err := judgeP.Chat(attemptCtx, judgeMessages, judgeSystemFor(phase), maxTokensForPhase(phase))
	attemptCancel()
	if err != nil {
		r.usage.RecordFailure(pe.Judge)
		if opened, until := r.recordProviderFailureForErr(pe.Judge, err); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Phase:    buildPhaseName(phase),
				Selected: pe.Judge,
				ModelID:  judgeModelID,
				Detail:   circuitOpenDetail(pe.Judge, time.Until(until)),
			})
		}
		r.emitTrace(TraceEvent{
			Event:      "judge_error",
			Phase:      buildPhaseName(phase),
			Selected:   pe.Judge,
			ModelID:    judgeModelID,
			DurationMS: time.Since(judgeStart).Milliseconds(),
			Error:      err.Error(),
		})
		return results[0]
	}
	r.recordProviderSuccess(pe.Judge)
	r.emitTrace(TraceEvent{
		Event:      "judge_success",
		Phase:      buildPhaseName(phase),
		Selected:   pe.Judge,
		ModelID:    judgeModelID,
		DurationMS: time.Since(judgeStart).Milliseconds(),
	})

	r.usage.Increment(pe.Judge)

	// Parse "WINNER: N" from first line for reliable winner attribution.
	// If parsing fails, default to the first result.
	winner := results[0]
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		firstLine := strings.TrimSpace(content[:idx])
		if strings.HasPrefix(firstLine, "WINNER:") {
			numStr := strings.TrimSpace(strings.TrimPrefix(firstLine, "WINNER:"))
			if n, parseErr := strconv.Atoi(numStr); parseErr == nil && n >= 1 && n <= len(results) {
				winner = results[n-1]
			}
		}
	}
	r.emitTrace(TraceEvent{
		Event:    "judge_winner_selected",
		Phase:    buildPhaseName(phase),
		Selected: winner.Provider,
		ModelID:  winner.ModelID,
	})
	r.usage.RecordQualityOutcome(phase, winner.Provider, true)

	// Return the winning generator's original content for correctness.
	// The judge's model ID and usage are recorded for cost tracking.
	return EnsembleResult{
		Provider: winner.Provider, // attribution: generator that was selected
		ModelID:  judgeModelID,    // judge's model (for cost/display)
		Content:  winner.Content,  // original generator output, not judge's reproduction
		Usage:    usage,
	}
}

// RecordQualityOutcome exposes the quality feedback mechanism to the app layer.
// Call with success=true when a provider's output passes validation (tests pass, LGTM review).
// Call with success=false when the output fails validation (auto-fix triggered, test failure).
func (r *Router) RecordQualityOutcome(phase BuildPhase, provider string, success bool) {
	r.usage.RecordQualityOutcome(phase, provider, success)
}

// judgeSystemFor returns a phase-specific system prompt for the judge.
func judgeSystemFor(phase BuildPhase) string {
	switch phase {
	case PhaseCode:
		return "You are an expert code evaluator. Given multiple implementations of the same task, select the most complete, correct, and well-structured one. Output ONLY the chosen implementation, completely unchanged, preserving all --- FILE: --- markers."
	case PhaseReview:
		return "You are an expert code reviewer. Given multiple code reviews, select or synthesize the most thorough and actionable one. Output ONLY the best review, completely unchanged."
	case PhasePlan:
		return "You are an expert software architect. Given multiple project plans, select the most detailed and feasible one. Output ONLY the chosen plan, completely unchanged."
	case PhaseFix:
		return "You are an expert debugger. Given multiple fixes for the same error, select the most correct and minimal fix. Output ONLY the chosen fix, completely unchanged, preserving all --- FILE: --- markers."
	default:
		return "You are an expert evaluator. Given multiple responses to the same request, select the best one. Output ONLY the chosen response, completely unchanged."
	}
}

// ChatBest selects the best provider response for a build phase.
//   - Power mode: runs all ensemble generators in parallel, then uses a cross-model
//     judge to select the winner.
//   - Other modes: uses Thompson Sampling to adaptively select the primary provider
//     from the buildStrategyTable candidates, then delegates to ChatWith.
func (r *Router) ChatBest(ctx context.Context, phase BuildPhase, messages []Message, system string, exclude ...string) (string, Usage, RouteResult, error) {
	if r.effectiveMode() != ModePower {
		return r.ChatWith(ctx, r.BuildProviderForAdaptive(phase), phase, messages, system, exclude...)
	}

	r.emitTrace(TraceEvent{
		Event:  "chat_best_power_start",
		Phase:  buildPhaseName(phase),
		Detail: "mode=power",
	})

	results := r.Ensemble(ctx, phase, messages, system, exclude...)
	if len(results) == 0 {
		// Ensemble had no available generators — fall back to adaptive routing
		r.emitTrace(TraceEvent{
			Event:  "chat_best_power_fallback_adaptive",
			Phase:  buildPhaseName(phase),
			Detail: "ensemble returned zero results",
		})
		return r.ChatWith(ctx, r.BuildProviderForAdaptive(phase), phase, messages, system, exclude...)
	}

	best := r.judgeSelect(ctx, phase, results)

	// Accumulate usage across all ensemble calls: generators + judge.
	var total Usage
	for _, res := range results {
		total.InputTokens += res.Usage.InputTokens
		total.OutputTokens += res.Usage.OutputTokens
		total.Cost += res.Usage.Cost
	}
	// Add judge's usage (previously missing).
	total.InputTokens += best.Usage.InputTokens
	total.OutputTokens += best.Usage.OutputTokens
	total.Cost += best.Usage.Cost
	total.Provider = best.Provider // winning generator
	total.Model = best.ModelID

	r.emitTrace(TraceEvent{
		Event:    "chat_best_power_selected",
		Phase:    buildPhaseName(phase),
		Selected: best.Provider,
		ModelID:  best.ModelID,
		Detail:   fmt.Sprintf("candidates=%d", len(results)),
	})

	return best.Content, total, RouteResult{
		Actual:    best.Provider,
		ModelID:   best.ModelID,
		Requested: "ensemble",
	}, nil
}
