package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// --- Task context propagation ---
// These helpers let CLI providers adapt behavior per-task (e.g. codex review vs exec).

type taskContextKey struct{}

// ContextWithTask returns a new context carrying the given TaskType.
func ContextWithTask(ctx context.Context, task TaskType) context.Context {
	return context.WithValue(ctx, taskContextKey{}, task)
}

// TaskFromContext retrieves the TaskType from context, if present.
func TaskFromContext(ctx context.Context) (TaskType, bool) {
	t, ok := ctx.Value(taskContextKey{}).(TaskType)
	return t, ok
}

type systemContextKey struct{}

// ContextWithSystem returns a new context carrying the system prompt.
// Used by CLI providers that support a dedicated system prompt flag.
func ContextWithSystem(ctx context.Context, system string) context.Context {
	return context.WithValue(ctx, systemContextKey{}, system)
}

// SystemFromContext retrieves the system prompt from context, if present.
func SystemFromContext(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(systemContextKey{}).(string)
	return s, ok && s != ""
}

type modelContextKey struct{}

// ContextWithModel returns a new context carrying the target model ID.
// CLI providers that support --model use this for per-call model selection.
func ContextWithModel(ctx context.Context, modelID string) context.Context {
	return context.WithValue(ctx, modelContextKey{}, modelID)
}

// ModelFromContext retrieves the model ID from context, if present and non-empty.
func ModelFromContext(ctx context.Context) (string, bool) {
	m, ok := ctx.Value(modelContextKey{}).(string)
	return m, ok && m != ""
}

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
var fallbackOrder = []string{"claude", "codex", "gemini"}

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

// ProviderEntry describes a pre-constructed provider with its access type.
type ProviderEntry struct {
	Provider Provider
	Access   AccessType
}

// RouterConfig provides configuration for creating a Router without depending
// on the config package. Library consumers use this directly.
type RouterConfig struct {
	// Providers maps provider names to pre-constructed provider instances.
	Providers map[string]ProviderEntry

	// Legacy model assignment (for routeLegacy when mode is not set).
	DefaultModel  string
	AnalysisModel string
	CodingModel   string
	ReviewModel   string

	// UsageMode is the initial mode ("fast", "balanced", "power").
	// Empty string means legacy routing (no mode).
	UsageMode string

	// ConfigDir is the directory for routing.json overrides and stats persistence.
	// Empty string disables file-based overrides.
	ConfigDir string
}

// providerKey is used to cache provider instances by (name, modelID).
type providerKey struct {
	name    string
	modelID string
}

// Router selects the best model provider for a given task.
type Router struct {
	// legacyModels stores model names for legacy (non-mode) routing.
	legacyModels struct {
		defaultModel  string
		analysisModel string
		codingModel   string
		reviewModel   string
	}

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

// NewRouterFromConfig creates a Router from a config-free RouterConfig.
// This is the primary constructor for library consumers.
func NewRouterFromConfig(rc RouterConfig) *Router {
	if rc.ConfigDir != "" {
		_ = LoadUserOverrides(rc.ConfigDir)
	}

	r := &Router{
		providers:     make(map[string]Provider),
		providerCache: make(map[providerKey]Provider),
		accessTypes:   make(map[string]AccessType),
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(defaultCircuitFailureThreshold, defaultCircuitCooldown),
	}

	r.legacyModels.defaultModel = rc.DefaultModel
	r.legacyModels.analysisModel = rc.AnalysisModel
	r.legacyModels.codingModel = rc.CodingModel
	r.legacyModels.reviewModel = rc.ReviewModel

	for name, entry := range rc.Providers {
		r.providers[name] = entry.Provider
		r.accessTypes[name] = entry.Access
	}

	if rc.UsageMode != "" {
		if mode, ok := ParseUsageMode(rc.UsageMode); ok {
			r.mode = mode
			r.modeSet = true
		}
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

// SetAccessType sets the access type for a named provider.
// This is used by the CLI adapter to apply explicit access overrides from config.
func (r *Router) SetAccessType(name string, access AccessType) {
	r.accessTypes[name] = access
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
// to ModeFast (the zero value) when the user has not chosen a mode.
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
		modelName = r.legacyModels.analysisModel
	case TaskCode, TaskFix:
		modelName = r.legacyModels.codingModel
	case TaskReview:
		modelName = r.legacyModels.reviewModel
	default:
		modelName = r.legacyModels.defaultModel
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
		if r.effectiveMode() == ModeFast {
			msg = "fast mode requires at least one provider; install a CLI or set an API key"
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

	ac := &attemptContext{
		ctx:       ContextWithTask(ctx, task),
		messages:  messages,
		system:    system,
		maxTokens: MaxTokensForTask(task),
		phase:     taskToBuildPhase(task),
		mode:      r.effectiveMode(),
		requested: result.Requested,
		task:      task,
		taskLabel: taskTypeName(task),
		labels: attemptLabels{
			attemptSuccess:  "chat_attempt_success",
			attemptError:    "chat_attempt_error",
			fallbackSkipped: "chat_fallback_skipped",
			failedAll:       "chat_failed_all",
		},
	}

	fallbacks := r.chatFallbackCandidates(task, result.Actual)

	// Choose the appropriate resolver based on routing mode
	var resolve candidateResolver
	if r.ModeSet() {
		resolve = r.resolverForMode()
	} else {
		resolve = r.legacyResolver()
	}

	return r.routeAndExecute(ac, result, fallbacks, resolve)
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
		if blocked, remaining := r.isCircuitOpen(name); blocked {
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

	fallbacks, tier := r.chatWithFallbackCandidates(phase, result.Actual, exclude)

	// Map build phase to task type for provider-specific behavior.
	taskForPhase := TaskCode
	switch phase {
	case PhaseReview:
		taskForPhase = TaskReview
	case PhasePlan:
		taskForPhase = TaskAnalyze
	case PhaseFix:
		taskForPhase = TaskFix
	}

	ac := &attemptContext{
		ctx:        ContextWithTask(ctx, taskForPhase),
		messages:   messages,
		system:     system,
		maxTokens:  maxTokensForPhase(phase),
		phase:      phase,
		mode:       r.effectiveMode(),
		requested:  name,
		phaseLabel: buildPhaseName(phase),
		labels: attemptLabels{
			attemptSuccess:  "build_chat_attempt_success",
			attemptError:    "build_chat_attempt_error",
			fallbackSkipped: "build_chat_fallback_skipped",
			failedAll:       "build_chat_failed_all",
		},
	}

	return r.routeAndExecute(ac, result, fallbacks, r.buildProviderResolver(tier))
}

// ChatStream sends a message and streams the response.
func (r *Router) ChatStream(ctx context.Context, task TaskType, messages []Message, system string) (<-chan StreamChunk, RouteResult, error) {
	result, err := r.Route(task)
	if err != nil {
		return nil, RouteResult{}, err
	}

	ac := &attemptContext{
		ctx:       ContextWithTask(ctx, task),
		messages:  messages,
		system:    system,
		maxTokens: MaxTokensForTask(task),
		phase:     taskToBuildPhase(task),
		mode:      r.effectiveMode(),
		requested: result.Requested,
		task:      task,
		taskLabel: taskTypeName(task),
		labels: attemptLabels{
			attemptSuccess:  "chat_stream_start_success",
			attemptError:    "chat_stream_start_error",
			fallbackSkipped: "chat_stream_fallback_skipped",
			failedAll:       "chat_stream_failed_all",
		},
	}

	fallbacks := r.chatFallbackCandidates(task, result.Actual)

	var resolve candidateResolver
	if r.ModeSet() {
		resolve = r.resolverForMode()
	} else {
		resolve = r.legacyResolver()
	}

	return r.routeAndExecuteStream(ac, result, fallbacks, resolve)
}
