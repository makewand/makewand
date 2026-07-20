package router

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
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

type usageModeContextKey struct{}

// ContextWithUsageMode returns a new context carrying the requested usage mode.
// Providers like the remote HTTP adapter use this to preserve client-side mode.
func ContextWithUsageMode(ctx context.Context, mode UsageMode) context.Context {
	return context.WithValue(ctx, usageModeContextKey{}, mode)
}

// UsageModeFromContext retrieves the requested usage mode, if present.
func UsageModeFromContext(ctx context.Context) (UsageMode, bool) {
	mode, ok := ctx.Value(usageModeContextKey{}).(UsageMode)
	return mode, ok
}

type workDirContextKey struct{}

// ContextWithWorkDir returns a new context carrying an override working
// directory for CLI providers.
func ContextWithWorkDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, workDirContextKey{}, strings.TrimSpace(dir))
}

// WorkDirFromContext retrieves the working directory override, if present.
func WorkDirFromContext(ctx context.Context) (string, bool) {
	dir, ok := ctx.Value(workDirContextKey{}).(string)
	return dir, ok && dir != ""
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

// providerModelReporter is implemented by transports that cannot necessarily
// honor the router's requested model ID. It keeps user-visible route metadata
// honest for provider-managed subscription CLIs.
type providerModelReporter interface {
	ReportedModelID(requestedModelID string) string
}

func reportedModelID(provider Provider, requestedModelID string) string {
	if reporter, ok := provider.(providerModelReporter); ok {
		if actual := strings.TrimSpace(reporter.ReportedModelID(requestedModelID)); actual != "" {
			return actual
		}
	}
	return requestedModelID
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

	// Quota, when set, makes routing subscription-quota aware: providers low on
	// headroom are deprioritized and confirmed-exhausted pools are skipped. Nil
	// disables quota awareness entirely (routing behaves as before).
	Quota QuotaController

	// QuotaPolicy overrides the default warn/crit thresholds. Zero value uses
	// DefaultQuotaPolicy.
	QuotaPolicy QuotaPolicy

	// RepoTrust controls untrusted-repository capability routing. The zero value
	// (RepoTrustTrusted) preserves existing behavior. RepoTrustUntrusted restricts
	// generation to providers that do not run a repo-aware agent on the host and
	// fails closed when none are available.
	RepoTrust RepoTrust
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

	// providerAllowlist restricts routing to a subset of providers for
	// request-scoped views such as remote token policies.
	providerAllowlist map[string]struct{}

	// tables holds this instance's routing/pricing tables, deep-copied from the
	// immutable package defaults at construction. Struct-literal Routers (tests)
	// leave it unset; routingTables() then lazily adopts their own private copy
	// of the defaults on first use, so they never read the shared, mutable
	// package-level defaultTables. Stored atomically so that lazy first-use is
	// race-free without a mutex on the hot routing path.
	tables atomic.Pointer[strategyTables]

	// Provider cache for mode-based routing (provider+model → instance)
	providerCache map[providerKey]Provider
	providerMu    sync.Mutex

	// factories maps provider names to per-instance factories for dynamic
	// model-specific construction (guarded by providerMu).
	factories map[string]ProviderFactory

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

	// Subscription-quota awareness (nil when disabled).
	quota       QuotaController
	quotaPolicy QuotaPolicy

	// repoTrust holds the untrusted-repository trust level as an int32 so it can
	// be read/written race-free on the hot routing path without a mutex. The zero
	// value is RepoTrustTrusted (existing behavior).
	repoTrust atomic.Int32

	// budgetReservations tracks in-flight, not-yet-ledgered spend per budgeted
	// scope so concurrent requests cannot each read the same month-to-date total
	// and collectively blow past a team budget. Zero value is ready to use.
	budgetReservations budgetReserver
}

// budgetReserver is the in-memory, per-scope spend accountant that makes budget
// admission atomic. For each budgeted scope it holds:
//
//   - committed: realized month-to-date spend. Seeded ONCE from the durable
//     usage ledger (the first time the scope is seen this process), then
//     maintained in memory as requests settle. Because admission reads this
//     in-memory value under the same lock that settles update, there is no
//     check-then-act gap against a stale ledger read.
//   - reserved: provisional spend for admitted-but-not-yet-settled requests.
//
// This bounds concurrent overshoot: simultaneous in-flight requests each reserve
// an estimate, and a later request cannot re-consume headroom that an earlier
// request already spent. (Single-process; multiple server instances sharing one
// DB are not strongly consistent — see SERVER_ALPHA.md.)
type budgetReserver struct {
	mu        sync.Mutex
	committed map[string]float64
	reserved  map[string]float64
	seeded    map[string]bool
}

// isSeeded reports whether the scope's committed total has been initialized.
func (b *budgetReserver) isSeeded(scope string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seeded[scope]
}

// seed initializes the scope's committed total from the durable ledger. The
// first caller wins; later callers are no-ops, so a concurrent settle that
// happened between an isSeeded check and this call is never clobbered.
func (b *budgetReserver) seed(scope string, ledgerSpend float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seeded[scope] {
		return
	}
	if b.committed == nil {
		b.committed = make(map[string]float64)
	}
	if b.seeded == nil {
		b.seeded = make(map[string]bool)
	}
	b.committed[scope] = sanitizeCost(ledgerSpend)
	b.seeded[scope] = true
}

// admit atomically checks headroom and, if there is room, reserves `estimate`.
// With a positive estimate it counts the request's OWN reservation in the
// headroom check (rejecting when committed+reserved+estimate would exceed
// budget), so an admitted request always keeps committed+reserved <= budget —
// meaning a request whose realized cost is <= its reservation can never push the
// scope over budget. estimate<=0 is a headroom-only check (reject at/over cap)
// with no reservation. Returns false (reserving nothing) when rejected.
func (b *budgetReserver) admit(scope string, budget, estimate float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	used := b.committed[scope] + b.reserved[scope]
	if estimate > 0 {
		if used+estimate > budget {
			return false
		}
		if b.reserved == nil {
			b.reserved = make(map[string]float64)
		}
		b.reserved[scope] += estimate
		return true
	}
	return used < budget
}

// settle finalizes an admitted request: it releases the reservation and folds
// the realized cost into committed, atomically, so the scope's spend is never
// momentarily uncounted. Call after the request's cost is known.
func (b *budgetReserver) settle(scope string, estimate, actual float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unreserveLocked(scope, estimate)
	if actual = sanitizeCost(actual); actual > 0 {
		if b.committed == nil {
			b.committed = make(map[string]float64)
		}
		b.committed[scope] += actual
	}
}

// sanitizeCost coerces a cost value into a safe, budget-usable number: NaN, ±Inf
// and negatives (which would poison the committed total or make comparisons
// always-false) become 0.
func sanitizeCost(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// unreserve drops a reservation without committing any cost. Used on rejection
// paths where a scope was reserved before a later scope failed.
func (b *budgetReserver) unreserve(scope string, estimate float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unreserveLocked(scope, estimate)
}

func (b *budgetReserver) unreserveLocked(scope string, estimate float64) {
	if estimate <= 0 || b.reserved == nil {
		return
	}
	b.reserved[scope] -= estimate
	if b.reserved[scope] <= 0 {
		delete(b.reserved, scope)
	}
}

const availCacheTTL = 10 * time.Second

// NewRouterFromConfig creates a Router from a config-free RouterConfig.
// This is the primary constructor for library consumers.
// It returns an error when the embedded strategy defaults are unusable or when
// rc.ConfigDir contains an invalid routing.json override.
func NewRouterFromConfig(rc RouterConfig) (*Router, error) {
	if initErr != nil {
		// Embedded strategy defaults failed to load — should not happen in a
		// correctly built binary. Surface it so callers can diagnose.
		return nil, initErr
	}

	r := &Router{
		providers:     make(map[string]Provider),
		providerCache: make(map[providerKey]Provider),
		factories:     make(map[string]ProviderFactory),
		accessTypes:   make(map[string]AccessType),
		usage:         newSessionUsage(),
		breaker:       newProviderCircuitBreaker(defaultCircuitFailureThreshold, defaultCircuitCooldown),
		quota:         rc.Quota,
		quotaPolicy:   rc.QuotaPolicy,
	}
	r.tables.Store(copyDefaultTables())
	r.repoTrust.Store(int32(rc.RepoTrust))
	// Propagate the trust level into a concrete quota snapshotter so its
	// background/forced refresh never execs a local CLI (e.g. `agy models`) to
	// probe quota for an unsafe provider in untrusted mode. A fake QuotaController
	// (tests) execs nothing, so only the concrete snapshotter needs this.
	if snap, ok := rc.Quota.(*QuotaSnapshotter); ok {
		snap.SetRepoTrust(rc.RepoTrust)
	}
	if r.quotaPolicy == (QuotaPolicy{}) {
		r.quotaPolicy = DefaultQuotaPolicy()
	}

	if rc.ConfigDir != "" {
		if err := r.routingTables().loadUserOverrides(rc.ConfigDir); err != nil {
			return nil, fmt.Errorf("load routing overrides: %w", err)
		}
	}

	r.legacyModels.defaultModel = rc.DefaultModel
	r.legacyModels.analysisModel = rc.AnalysisModel
	r.legacyModels.codingModel = rc.CodingModel
	r.legacyModels.reviewModel = rc.ReviewModel

	for name, entry := range rc.Providers {
		r.providers[name] = entry.Provider
		r.accessTypes[name] = entry.Access
		// Price non-streaming responses from this Router's snapshot, not the
		// package defaults, so pricing overrides apply to pre-built providers.
		r.attachCostTable(entry.Provider)
	}

	if rc.UsageMode != "" {
		if mode, ok := ParseUsageMode(rc.UsageMode); ok {
			r.mode = mode
			r.modeSet = true
		}
	}

	return r, nil
}

// routingTables returns this Router's strategy tables. Routers constructed via
// struct literal (tests) have no instance tables yet; on first use they lazily
// adopt their own private copy of the immutable defaults, so they never read the
// shared, mutable package-level defaultTables (which the deprecated package-level
// LoadUserOverrides can mutate).
func (r *Router) routingTables() *strategyTables {
	if t := r.tables.Load(); t != nil {
		return t
	}
	// Lazily initialize from a fresh copy of the immutable defaults. CAS makes a
	// concurrent first-use race-free without a mutex, avoiding lock-ordering
	// hazards on the hot routing path (routingTables is called under other locks).
	fresh := copyDefaultTables()
	if r.tables.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return r.tables.Load()
}

// LoadUserOverrides deep-merges configDir/routing.json into this Router's
// tables. Missing file is not an error; invalid overrides leave the tables
// unchanged and return the error.
func (r *Router) LoadUserOverrides(configDir string) error {
	return r.routingTables().loadUserOverrides(configDir)
}

// ContextBudgetForMode returns this Router's context budget (in chars) for the
// first provider of the (mode, task) strategy entry, reading this instance's
// tables (including any user overrides). Prefer this over the deprecated
// package-level ContextBudgetForMode, which only sees the package defaults.
func (r *Router) ContextBudgetForMode(mode UsageMode, task TaskType) int {
	return r.routingTables().contextBudgetForMode(mode, task)
}

// ContextBudgetForProvider returns this Router's context budget (in chars) for
// the given provider and tier, reading this instance's tables.
func (r *Router) ContextBudgetForProvider(provider string, tier ModelTier) int {
	return r.routingTables().contextBudget(provider, tier)
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
	r.providerMu.Lock()
	r.accessTypes[name] = access
	r.providerMu.Unlock()
}

// getAccessType returns the access type for a named provider (thread-safe).
func (r *Router) getAccessType(name string) AccessType {
	r.providerMu.Lock()
	defer r.providerMu.Unlock()
	return r.accessTypes[name]
}

// StartQuotaRefresh begins periodic background refresh of subscription-quota
// headroom, if a live snapshotter is wired. It is the composition root's job to
// own the lifetime: pass a context tied to the process (a signal-derived one for
// serve, the program context for the TUI) so the ticker stops on shutdown.
//
// Idempotent and cancellable — repeated calls never spawn a second ticker. A
// no-op when quota awareness is disabled or the controller is a test fake with
// no refresh loop. Construction stays network-free; nothing starts a refresh on
// its own, which is why long-running entrypoints must call this explicitly.
func (r *Router) StartQuotaRefresh(ctx context.Context) {
	if r == nil || r.quota == nil {
		return
	}
	if lc, ok := r.quota.(interface{ Start(context.Context) }); ok {
		lc.Start(ctx)
	}
}

// RefreshQuota forces one synchronous quota refresh and returns when it lands.
// Use it where readiness matters (a health/doctor probe) rather than relying on
// the async loop's first tick. A no-op when quota awareness is disabled.
func (r *Router) RefreshQuota(ctx context.Context) {
	if r == nil || r.quota == nil {
		return
	}
	if rf, ok := r.quota.(interface {
		Refresh(context.Context) *QuotaSnapshot
	}); ok {
		rf.Refresh(ctx)
	}
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

func (r *Router) cloneView() *Router {
	r.providerMu.Lock()
	providers := make(map[string]Provider, len(r.providers))
	for name, provider := range r.providers {
		providers[name] = provider
	}
	accessTypes := make(map[string]AccessType, len(r.accessTypes))
	for name, access := range r.accessTypes {
		accessTypes[name] = access
	}
	providerCache := make(map[providerKey]Provider, len(r.providerCache))
	for key, provider := range r.providerCache {
		providerCache[key] = provider
	}
	factories := make(map[string]ProviderFactory, len(r.factories))
	for name, factory := range r.factories {
		factories[name] = factory
	}
	r.providerMu.Unlock()

	r.mu.Lock()
	cachedAvail := append([]string(nil), r.cachedAvail...)
	cachedAvailAt := r.cachedAvailAt
	legacy := r.legacyModels
	r.mu.Unlock()

	r.modeMu.RLock()
	mode := r.mode
	modeSet := r.modeSet
	r.modeMu.RUnlock()

	r.traceMu.RLock()
	traceSink := r.traceSink
	r.traceMu.RUnlock()

	clone := &Router{
		legacyModels:      legacy,
		providers:         providers,
		providerAllowlist: cloneStringSet(r.providerAllowlist),
		providerCache:     providerCache,
		factories:         factories,
		accessTypes:       accessTypes,
		usage:             r.usage,
		breaker:           r.breaker,
		quota:             r.quota,
		quotaPolicy:       r.quotaPolicy,
		mode:              mode,
		modeSet:           modeSet,
		cachedAvail:       cachedAvail,
		cachedAvailAt:     cachedAvailAt,
		traceSink:         traceSink,
	}
	// Per-request views share the parent's live tables snapshot so hot-reloads
	// are visible through clones too.
	clone.tables.Store(r.routingTables())
	// Inherit the trust level so untrusted-repo enforcement survives per-request
	// views (mode/allowlist clones) rather than silently reverting to trusted.
	clone.repoTrust.Store(int32(r.RepoTrust()))
	return clone
}

// cloneWithMode creates a per-request router view that preserves shared
// provider state while allowing a different routing mode without mutating
// the live server router.
func (r *Router) cloneWithMode(mode UsageMode) *Router {
	clone := r.cloneView()
	clone.mode = mode
	clone.modeSet = true
	return clone
}

func (r *Router) cloneWithProviderAllowlist(names []string) *Router {
	clone := r.cloneView()
	clone.setProviderAllowlist(names)
	return clone
}

func (r *Router) setProviderAllowlist(names []string) {
	r.providerAllowlist = makeStringSet(names)
	r.mu.Lock()
	r.cachedAvail = nil
	r.cachedAvailAt = time.Time{}
	r.mu.Unlock()
}

func (r *Router) isProviderAllowed(name string) bool {
	if len(r.providerAllowlist) == 0 {
		return true
	}
	_, ok := r.providerAllowlist[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func makeStringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		out[value] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneStringSet(values map[string]struct{}) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for value := range values {
		out[value] = struct{}{}
	}
	return out
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
	if name, provider, ok := r.remoteOnlyProvider(); ok {
		return RouteResult{
			Provider:  provider,
			Requested: name,
			Actual:    name,
		}, nil
	}
	if r.ModeSet() {
		return r.routeByMode(task)
	}
	return r.routeLegacy(task)
}

// legacyModelID returns the best-effort model ID for a provider in legacy mode.
// Legacy routing has no tier concept, so we use TierMid as a reasonable default.
func (r *Router) legacyModelID(providerName string) string {
	return r.routingTables().modelID(providerName, TierMid)
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

	// Try the preferred model. The capability check runs BEFORE IsAvailable so an
	// unsafe CLI is never probed (IsAvailable execs a host health check) in
	// untrusted mode. In trusted mode untrustedRepoSafe is always true, so the
	// unsafe branch is dead and IsAvailable gates exactly as before.
	if p, ok := r.getProvider(modelName); ok {
		switch {
		case !r.untrustedRepoSafe(p):
			r.emitTrace(TraceEvent{
				Event:     "route_candidate_skipped",
				Task:      taskTypeName(task),
				Requested: modelName,
				Selected:  modelName,
				ModelID:   r.legacyModelID(modelName),
				Detail:    untrustedRepoSkipDetail(modelName),
			})
		case !p.IsAvailable():
			// Not available — fall through to the deterministic fallback below.
		default:
			if blocked, remaining := r.isCircuitOpen(modelName); blocked {
				r.emitTrace(TraceEvent{
					Event:     "route_candidate_skipped",
					Task:      taskTypeName(task),
					Requested: modelName,
					Selected:  modelName,
					ModelID:   r.legacyModelID(modelName),
					Detail:    circuitOpenDetail(modelName, remaining),
				})
			} else {
				modelID := r.legacyModelID(modelName)
				r.emitTrace(TraceEvent{
					Event:     "route_selected",
					Task:      taskTypeName(task),
					Requested: modelName,
					Selected:  modelName,
					ModelID:   modelID,
				})
				return RouteResult{
					Provider:  p,
					ModelID:   modelID,
					Requested: modelName,
					Actual:    modelName,
				}, nil
			}
		}
	}

	// Deterministic fallback: try the subscription provider's API alias first,
	// then the remaining providers in defined order. The capability check runs
	// BEFORE IsAvailable so an unsafe CLI is never probed in untrusted mode; in
	// trusted mode untrustedRepoSafe is always true so gating is unchanged.
	for _, c := range r.legacyFallbackCandidates(modelName) {
		p, ok := r.getProvider(c.name)
		if !ok {
			continue
		}
		if !r.untrustedRepoSafe(p) {
			r.emitTrace(TraceEvent{
				Event:      "route_candidate_skipped",
				Task:       taskTypeName(task),
				Requested:  modelName,
				Selected:   c.name,
				ModelID:    c.modelID,
				IsFallback: true,
				Detail:     untrustedRepoSkipDetail(c.name),
			})
			continue
		}
		if !p.IsAvailable() {
			continue
		}
		if blocked, remaining := r.isCircuitOpen(c.name); blocked {
			r.emitTrace(TraceEvent{
				Event:      "route_candidate_skipped",
				Task:       taskTypeName(task),
				Requested:  modelName,
				Selected:   c.name,
				ModelID:    c.modelID,
				IsFallback: true,
				Detail:     circuitOpenDetail(c.name, remaining),
			})
			continue
		}
		r.emitTrace(TraceEvent{
			Event:      "route_selected",
			Task:       taskTypeName(task),
			Requested:  modelName,
			Selected:   c.name,
			ModelID:    c.modelID,
			IsFallback: true,
		})
		return RouteResult{
			Provider:   p,
			ModelID:    c.modelID,
			Requested:  modelName,
			Actual:     c.name,
			IsFallback: true,
		}, nil
	}

	if r.RepoTrust() == RepoTrustUntrusted {
		r.emitTrace(TraceEvent{
			Event:     "route_failed",
			Task:      taskTypeName(task),
			Requested: modelName,
			Error:     ErrNoUntrustedSafeProvider.Error(),
		})
		return RouteResult{}, ErrNoUntrustedSafeProvider
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
		if r.RepoTrust() == RepoTrustUntrusted {
			r.emitTrace(TraceEvent{
				Event:     "route_failed",
				Task:      taskTypeName(task),
				Phase:     buildPhaseName(phase),
				Requested: requested,
				Error:     ErrNoUntrustedSafeProvider.Error(),
			})
			return RouteResult{}, ErrNoUntrustedSafeProvider
		}
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
		if !r.untrustedRepoSafe(p) {
			r.emitTrace(TraceEvent{
				Event:     "route_candidate_skipped",
				Task:      taskTypeName(task),
				Phase:     buildPhaseName(phase),
				Requested: requested,
				Selected:  c.name,
				ModelID:   c.modelID,
				Detail:    untrustedRepoSkipDetail(c.name),
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

	if r.RepoTrust() == RepoTrustUntrusted {
		r.emitTrace(TraceEvent{
			Event:     "route_failed",
			Task:      taskTypeName(task),
			Phase:     buildPhaseName(phase),
			Requested: requested,
			Error:     ErrNoUntrustedSafeProvider.Error(),
		})
		return RouteResult{}, ErrNoUntrustedSafeProvider
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
			p, modelID, resolveErr := r.tryBuildProvider(name, bs.Tier)
			switch {
			case resolveErr == nil && r.untrustedRepoSafe(p) && p.IsAvailable():
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
			case resolveErr != nil:
				r.emitTrace(TraceEvent{
					Event:     "build_route_candidate_skipped",
					Phase:     buildPhaseName(phase),
					Requested: name,
					Selected:  name,
					Error:     resolveErr.Error(),
				})
			case !r.untrustedRepoSafe(p):
				r.emitTrace(TraceEvent{
					Event:     "build_route_candidate_skipped",
					Phase:     buildPhaseName(phase),
					Requested: name,
					Selected:  name,
					ModelID:   modelID,
					Detail:    untrustedRepoSkipDetail(name),
				})
			default:
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
		if !r.untrustedRepoSafe(p) {
			r.emitTrace(TraceEvent{
				Event:      "build_route_candidate_skipped",
				Phase:      buildPhaseName(phase),
				Requested:  name,
				Selected:   fb,
				ModelID:    modelID,
				IsFallback: true,
				Detail:     untrustedRepoSkipDetail(fb),
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

	if r.RepoTrust() == RepoTrustUntrusted {
		r.emitTrace(TraceEvent{
			Event:     "build_route_failed",
			Phase:     buildPhaseName(phase),
			Requested: name,
			Error:     ErrNoUntrustedSafeProvider.Error(),
		})
		return RouteResult{}, ErrNoUntrustedSafeProvider
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

// ChatStreamWith streams a response from a specific provider for a build phase.
// It mirrors ChatWith but preserves streaming semantics for HTTP and remote use.
func (r *Router) ChatStreamWith(ctx context.Context, name string, phase BuildPhase, messages []Message, system string, exclude ...string) (<-chan StreamChunk, RouteResult, error) {
	result, err := r.RouteProvider(name, phase, exclude...)
	if err != nil {
		return nil, RouteResult{}, err
	}

	fallbacks, tier := r.chatWithFallbackCandidates(phase, result.Actual, exclude)

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
			attemptSuccess:  "build_chat_stream_start_success",
			attemptError:    "build_chat_stream_start_error",
			fallbackSkipped: "build_chat_stream_fallback_skipped",
			failedAll:       "build_chat_stream_failed_all",
		},
	}

	return r.routeAndExecuteStream(ac, result, fallbacks, r.buildProviderResolver(tier))
}
