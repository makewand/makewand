package model

import (
	"context"
	"fmt"
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
	TaskExplain                 // explanation, summarization
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
// It loads user routing overrides from configDir/routing.json if present.
func NewRouter(cfg *config.Config) *Router {
	// Load user routing overrides (best-effort; errors are silently ignored).
	if dir, err := config.ConfigDir(); err == nil {
		_ = LoadUserOverrides(dir)
	}

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
		r.providers[name] = NewCommandCLI(name, command, cp.Args, config.EffectiveCustomProviderPromptMode(cp))

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

// Chat sends a message using the best provider for the given task type.
// If the primary provider fails, it tries the next available provider.
func (r *Router) Chat(ctx context.Context, task TaskType, messages []Message, system string) (string, Usage, RouteResult, error) {
	result, err := r.Route(task)
	if err != nil {
		return "", Usage{}, RouteResult{}, err
	}
	maxTokens := MaxTokensForTask(task)
	attemptPhase := taskToBuildPhase(task)
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
		attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), attemptPhase, result.Actual)
		content, usage, chatErr := result.Provider.Chat(attemptCtx, messages, system, maxTokens)
		attemptCancel()
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
				attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), attemptPhase, c.name)
				content, usage, retryErr := p.Chat(attemptCtx, messages, system, maxTokens)
				attemptCancel()
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
		attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), attemptPhase, name)
		content, usage, retryErr := p.Chat(attemptCtx, messages, system, maxTokens)
		attemptCancel()
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
		attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), phase, result.Actual)
		content, usage, chatErr := result.Provider.Chat(attemptCtx, messages, system, maxTokens)
		attemptCancel()
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
		attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), phase, fb)
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
	attemptPhase := taskToBuildPhase(task)
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
		attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), attemptPhase, result.Actual)
		ch, streamErr := result.Provider.ChatStream(attemptCtx, messages, system, maxTokens)
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
			return withStreamAttemptCancel(ch, attemptCancel), result, nil
		}
		attemptCancel()
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
				attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), attemptPhase, c.name)
				ch, retryErr := p.ChatStream(attemptCtx, messages, system, maxTokens)
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
					return withStreamAttemptCancel(ch, attemptCancel), RouteResult{
						Provider:   p,
						ModelID:    c.modelID,
						Requested:  result.Requested,
						Actual:     c.name,
						IsFallback: true,
					}, nil
				}
				attemptCancel()
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
		attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), attemptPhase, name)
		ch, retryErr := p.ChatStream(attemptCtx, messages, system, maxTokens)
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
			return withStreamAttemptCancel(ch, attemptCancel), RouteResult{
				Provider:   p,
				Requested:  result.Requested,
				Actual:     name,
				IsFallback: true,
			}, nil
		}
		attemptCancel()
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
