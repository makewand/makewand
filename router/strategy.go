package router

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// UsageMode controls the model selection strategy (quality/cost tradeoff).
type UsageMode int

const (
	ModeFast     UsageMode = iota // Fast: cheap tier, prefer free/subscription providers
	_                             // reserved (was ModeEconomy)
	ModeBalanced                  // Balanced: mid tier, good quality/cost ratio
	ModePower                     // Power: premium tier, best quality
)

// AccessType describes how a provider is accessed.
type AccessType int

const (
	AccessFree         AccessType = iota // Free tier (e.g. Gemini Flash)
	AccessLocal                          // Local model
	AccessSubscription                   // Paid subscription tier
	AccessAPI                            // Pay-per-token API
)

// ModelTier categorizes model quality levels.
type ModelTier int

const (
	TierCheap   ModelTier = iota // Fast/cheap models
	TierMid                      // Good balance
	TierPremium                  // Best quality
)

// priceCompletion returns the USD cost for a completion priced from table t.
// A nil t falls back to the package-level default tables, so providers created
// outside a Router (for example a direct NewClaude caller) still get a
// best-effort price. Reads are lock-guarded by costFor, so a live Router
// snapshot swapped in by a hot-reload is picked up automatically.
func priceCompletion(t *strategyTables, modelID string, inputTokens, outputTokens int) float64 {
	if t == nil {
		t = defaultTables
	}
	entry, ok := t.costFor(modelID)
	if !ok {
		return 0
	}
	return float64(inputTokens)/1_000_000*entry.Input + float64(outputTokens)/1_000_000*entry.Output
}

// EstimateCost returns the estimated cost in USD for the given model and token
// counts, priced from the package-level default tables. Providers constructed
// through a Router price from that Router's own snapshot instead (see
// instanceCostTable); this remains for direct, routerless callers.
func EstimateCost(modelID string, inputTokens, outputTokens int) float64 {
	return priceCompletion(defaultTables, modelID, inputTokens, outputTokens)
}

// costTableContextKey carries the owning Router's per-call price snapshot.
type costTableContextKey struct{}

// contextWithCostTable returns a context carrying the Router's price snapshot so
// a provider prices the completion from the calling Router's overrides — even
// when the same provider instance is shared by several Routers.
func contextWithCostTable(ctx context.Context, t *strategyTables) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, costTableContextKey{}, t)
}

// costTableFromContext retrieves the per-call price snapshot, if present.
func costTableFromContext(ctx context.Context) (*strategyTables, bool) {
	t, ok := ctx.Value(costTableContextKey{}).(*strategyTables)
	return t, ok && t != nil
}

// instanceCostTable is embedded by API providers (Claude, Gemini, OpenAI) so a
// Router can price a completion from its per-instance snapshot.
//
// Pricing is a function of (per-call snapshot, model): priceForCtx reads the
// snapshot the owning Router injects into the request context, so a provider
// instance SHARED by two Routers prices each Router's calls from that Router's
// overrides. The embedded atomic pointer is only a fallback for direct,
// routerless callers (and is set via useCostTable at registration time).
type instanceCostTable struct {
	tables atomic.Pointer[strategyTables]
}

// useCostTable stores a fallback price snapshot for routerless callers. Router
// requests carry their own snapshot in the context (see priceForCtx), so this
// pointer is not consulted on the router-driven Chat path and a shared provider
// registered on two Routers is never mispriced by a last-writer-wins overwrite.
func (c *instanceCostTable) useCostTable(t *strategyTables) { c.tables.Store(t) }

// priceForCtx prices a completion, preferring the per-call snapshot injected into
// ctx by the owning Router; it falls back to the registration-time snapshot, then
// the package defaults. Providers call this from Chat so pricing follows the
// calling Router even when the provider instance is shared across Routers.
func (c *instanceCostTable) priceForCtx(ctx context.Context, modelID string, inputTokens, outputTokens int) float64 {
	if t, ok := costTableFromContext(ctx); ok {
		return priceCompletion(t, modelID, inputTokens, outputTokens)
	}
	return priceCompletion(c.tables.Load(), modelID, inputTokens, outputTokens)
}

// priceFor prices a completion from the registration-time snapshot, or the
// package defaults when none was injected. Retained for direct, routerless
// callers that price without a request context.
func (c *instanceCostTable) priceFor(modelID string, inputTokens, outputTokens int) float64 {
	return priceCompletion(c.tables.Load(), modelID, inputTokens, outputTokens)
}

// ParseUsageMode converts a string to UsageMode.
func ParseUsageMode(s string) (UsageMode, bool) {
	switch strings.ToLower(s) {
	case "fast":
		return ModeFast, true
	case "balanced":
		return ModeBalanced, true
	case "power":
		return ModePower, true
	default:
		return 0, false
	}
}

// String returns the display name for the mode.
func (m UsageMode) String() string {
	switch m {
	case ModeFast:
		return "fast"
	case ModeBalanced:
		return "balanced"
	case ModePower:
		return "power"
	default:
		return "unknown"
	}
}

// ParseAccessType determines the AccessType from a config value and provider name.
// Defaults: all built-in providers default to Subscription (CLI tools preferred).
func ParseAccessType(configValue, providerName string) AccessType {
	switch strings.ToLower(configValue) {
	case "free":
		return AccessFree
	case "local":
		return AccessLocal
	case "subscription":
		return AccessSubscription
	case "api":
		return AccessAPI
	}
	// Default: subscription (CLI tools are the primary access method).
	return AccessSubscription
}

// accessPriority returns the sort weight for access types.
// Lower is preferred: Subscription(0) < Free(1) < Local(2) < API(3).
// Subscription is intentionally preferred ahead of free tiers so the router
// spends the user's flat-rate quota before it spends per-token API budget.
func accessPriority(at AccessType) int {
	switch at {
	case AccessSubscription:
		return 0
	case AccessFree:
		return 1
	case AccessLocal:
		return 2
	case AccessAPI:
		return 3
	default:
		return 4
	}
}

// qualityKey indexes per-(phase, provider) Beta distribution parameters.
type qualityKey struct {
	phase    BuildPhase
	provider string
}

// providerQuality holds the α/β parameters of a Beta distribution for Thompson Sampling.
// α (Successes) grows when a provider is chosen by a cross-model judge or passes validation.
// β (Failures) grows when generated code requires an auto-fix or causes test failures.
// Fields are exported so they can be round-tripped through JSON for cross-session persistence.
type providerQuality struct {
	Successes float64 `json:"successes"` // alpha
	Failures  float64 `json:"failures"`  // beta
}

// sessionUsage tracks per-provider request counts, error-failure counts, and
// per-phase quality distributions used for Thompson Sampling–based routing.
type sessionUsage struct {
	mu        sync.Mutex
	counts    map[string]int
	failures  map[string]int                  // provider errors / timeouts
	quality   map[qualityKey]*providerQuality // Beta(α,β) per (phase, provider)
	rng       *rand.Rand                      // injectable random source for deterministic tests
	mutations uint64                          // bumped on every recorded change; drives save debouncing

	// saveMu serializes Save calls so concurrent requests never race the
	// temp-file rename. It is separate from mu so recording stats never blocks
	// on disk I/O. savedMutations and lastSaveAt are guarded by saveMu.
	saveMu         sync.Mutex
	savedMutations uint64
	lastSaveAt     time.Time
}

func newSessionUsage() *sessionUsage {
	return &sessionUsage{
		counts:   make(map[string]int),
		failures: make(map[string]int),
		quality:  make(map[qualityKey]*providerQuality),
		rng:      rand.New(rand.NewSource(rand.Int63())), //nolint:gosec // G404: Thompson-sampling exploration jitter, not security-sensitive randomness.
	}
}

func (s *sessionUsage) Increment(provider string) {
	s.mu.Lock()
	s.counts[provider]++
	s.mutations++
	s.mu.Unlock()
}

func (s *sessionUsage) Count(provider string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[provider]
}

// RecordFailure records a provider error/timeout (distinct from quality failures).
func (s *sessionUsage) RecordFailure(provider string) {
	s.mu.Lock()
	s.failures[provider]++
	s.mutations++
	s.mu.Unlock()
}

// FailureCount returns the number of API errors recorded for the given provider.
func (s *sessionUsage) FailureCount(provider string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures[provider]
}

// FailureRate returns the fraction of requests that errored for the given provider.
func (s *sessionUsage) FailureRate(provider string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.counts[provider] + s.failures[provider]
	if total == 0 {
		return 0
	}
	return float64(s.failures[provider]) / float64(total)
}

// QualitySampleCount returns the number of recorded quality outcomes for
// (phase, provider), i.e. successes + failures in the Beta posterior.
func (s *sessionUsage) QualitySampleCount(phase BuildPhase, provider string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := qualityKey{phase: phase, provider: provider}
	q := s.quality[key]
	if q == nil {
		return 0
	}
	return int(q.Successes + q.Failures)
}

// RecordQualityOutcome records a quality signal for (phase, provider).
//   - success=true:  provider was selected by a cross-model judge, or its code passed tests.
//   - success=false: provider's code required an auto-fix, or it lost a judge comparison.
func (s *sessionUsage) RecordQualityOutcome(phase BuildPhase, provider string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := qualityKey{phase, provider}
	if s.quality[key] == nil {
		s.quality[key] = &providerQuality{}
	}
	if success {
		s.quality[key].Successes++
	} else {
		s.quality[key].Failures++
	}
	s.mutations++
}

// ThompsonSample draws a score from Beta(α, β) for (phase, provider).
//
// priorBias seeds α with virtual successes proportional to the provider's position
// in the static strategy table (position 0 → bias 2.0, position 1 → 1.0, 2+ → 0.0),
// so the static table acts as a Bayesian prior that gets overridden by observed quality.
func (s *sessionUsage) ThompsonSample(phase BuildPhase, provider string, priorBias float64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := qualityKey{phase, provider}
	q := s.quality[key]
	var alpha, beta float64
	if q != nil {
		alpha = q.Successes + priorBias + 1.0
		beta = q.Failures + 1.0
	} else {
		alpha = priorBias + 1.0
		beta = 1.0
	}
	return betaSample(s.rng, alpha, beta)
}

// betaSample draws a random variate from Beta(alpha, beta) via the Gamma relationship.
func betaSample(rng *rand.Rand, alpha, beta float64) float64 {
	x := gammaSample(rng, alpha)
	y := gammaSample(rng, beta)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaSample draws from Gamma(alpha, 1) using Marsaglia-Tsang's method.
func gammaSample(rng *rand.Rand, alpha float64) float64 {
	if alpha < 1.0 {
		return gammaSample(rng, 1.0+alpha) * math.Pow(rng.Float64(), 1.0/alpha)
	}
	d := alpha - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		var x, v float64
		for {
			x = rng.NormFloat64()
			v = 1.0 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := rng.Float64()
		x2 := x * x
		if u < 1.0-0.0331*(x2*x2) {
			return d * v
		}
		if math.Log(u) < 0.5*x2+d*(1.0-v+math.Log(v)) {
			return d * v
		}
	}
}

// taskToBuildPhase maps a TaskType to the canonical BuildPhase for quality tracking.
func taskToBuildPhase(t TaskType) BuildPhase {
	switch t {
	case TaskCode:
		return PhaseCode
	case TaskReview:
		return PhaseReview
	case TaskFix:
		return PhaseFix
	default:
		return PhasePlan
	}
}

// candidate is a provider candidate for mode-based routing.
type candidate struct {
	name           string
	modelID        string
	access         AccessType
	order          int       // original position in strategy table
	useCount       int       // session success count
	failureRate    float64   // fraction of requests that errored this session
	requests       int       // total requests (counts + failures) for min-sample gate
	qualitySamples int       // total quality outcomes (success + failure) for this phase
	thompsonScore  float64   // sampled from Beta(α,β); higher → higher priority
	quotaBand      QuotaBand // subscription headroom band; lower (OK) sorts first
}

// minSamplesForExclusion is the default minimum number of total requests
// (success + error) required before the hard >50% error-failure-rate exclusion
// rule applies.
const minSamplesForExclusion = 5

// minSamplesForFastDegrade is used by fast/power mode to react faster to
// unstable providers (for example repeated CLI auth/timeout failures).
const minSamplesForFastDegrade = 2

// sortCandidates sorts candidates by:
//  1. Access type priority (Free/Subscription < Local < API)
//  2. Hard exclusion: >50% error-failure rate with ≥5 total requests
//  3. Thompson Sampling score (adaptive quality signal seeded by static table priors)
//  4. Session use count (load balance among equal-score candidates)
//  5. Original strategy table order as a stable tiebreaker
func sortCandidates(candidates []candidate) {
	sortCandidatesWithMinSamples(candidates, minSamplesForExclusion)
}

func sortCandidatesForMode(candidates []candidate, mode UsageMode) {
	minSamples := minSamplesForExclusion
	if mode == ModeFast || mode == ModePower {
		minSamples = minSamplesForFastDegrade
	}
	sortCandidatesWithMinSamples(candidates, minSamples)
}

func sortCandidatesWithMinSamples(candidates []candidate, minSamples int) {
	if minSamples <= 0 {
		minSamples = 1
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		pi, pj := accessPriority(candidates[i].access), accessPriority(candidates[j].access)
		if pi != pj {
			return pi < pj
		}
		// Hard-exclude providers with >50% error-failure rate, but only after
		// enough samples to avoid false-positives from transient errors.
		hi := candidates[i].failureRate > 0.5 && candidates[i].requests >= minSamples
		hj := candidates[j].failureRate > 0.5 && candidates[j].requests >= minSamples
		if hi != hj {
			return !hi
		}
		// Subscription-quota band: prefer providers with more headroom. This is
		// an orthogonal coarse key placed above the quality signal so a nearly
		// exhausted pool is tried last, but Thompson still orders within a band.
		if candidates[i].quotaBand != candidates[j].quotaBand {
			return candidates[i].quotaBand < candidates[j].quotaBand
		}
		// Cold-start stability: when neither candidate has requests or quality
		// outcomes, prefer static strategy order over random Thompson variance.
		if candidates[i].requests == 0 && candidates[j].requests == 0 &&
			candidates[i].qualitySamples == 0 && candidates[j].qualitySamples == 0 {
			return candidates[i].order < candidates[j].order
		}
		// Thompson score is the primary quality signal.
		si, sj := candidates[i].thompsonScore, candidates[j].thompsonScore
		if math.Abs(si-sj) > 0.001 {
			return si > sj
		}
		// Tiebreaker: prefer the less-used provider for load balancing.
		if candidates[i].useCount != candidates[j].useCount {
			return candidates[i].useCount < candidates[j].useCount
		}
		return candidates[i].order < candidates[j].order
	})
}

// statsFile is the filename for cross-session routing quality statistics.
const statsFile = "routing_stats.json"

// persistedStats is the JSON schema for routing_stats.json.
type persistedStats struct {
	Version  int                         `json:"version"`
	Counts   map[string]int              `json:"counts"`
	Failures map[string]int              `json:"failures"`
	Quality  map[string]*providerQuality `json:"quality"` // key: "<phase>:<provider>"
}

// minStatsSaveInterval is the minimum time between debounced stats writes.
// Explicit Save calls always flush regardless of the interval.
const minStatsSaveInterval = time.Second

// Save writes the session's routing statistics to configDir/routing_stats.json.
// Existing data is replaced; the file is written atomically via a temp file.
// Concurrent Save calls are serialized, so this is safe to call per-request.
func (s *sessionUsage) Save(configDir string) error {
	return s.save(configDir, true)
}

// saveDebounced is like Save but skips the write when nothing changed since the
// last save or when the last write was less than minStatsSaveInterval ago.
// Hot per-request paths use it so stats persistence never becomes a disk storm.
func (s *sessionUsage) saveDebounced(configDir string) error {
	return s.save(configDir, false)
}

func (s *sessionUsage) save(configDir string, force bool) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	mutations := s.mutations
	if !force && (mutations == s.savedMutations || time.Since(s.lastSaveAt) < minStatsSaveInterval) {
		s.mu.Unlock()
		return nil
	}
	data := persistedStats{
		Version:  1,
		Counts:   make(map[string]int, len(s.counts)),
		Failures: make(map[string]int, len(s.failures)),
		Quality:  make(map[string]*providerQuality, len(s.quality)),
	}
	for k, v := range s.counts {
		data.Counts[k] = v
	}
	for k, v := range s.failures {
		data.Failures[k] = v
	}
	for k, v := range s.quality {
		key := strconv.Itoa(int(k.phase)) + ":" + k.provider
		cp := *v
		data.Quality[key] = &cp
	}
	s.mu.Unlock()

	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	// Unique temp file in the target dir: saveMu serializes this instance, but a
	// fixed temp name could still collide with another process writing the same
	// configDir. CreateTemp files are 0600 by default.
	tmp, err := os.CreateTemp(configDir, statsFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(configDir, statsFile)); err != nil {
		os.Remove(tmpPath)
		return err
	}
	s.savedMutations = mutations
	s.lastSaveAt = time.Now()
	return nil
}

// Load reads cross-session routing statistics from configDir/routing_stats.json
// and merges them into the current session. Missing or corrupt files are silently ignored.
func (s *sessionUsage) Load(configDir string) error {
	b, err := os.ReadFile(filepath.Join(configDir, statsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var data persistedStats
	if err := json.Unmarshal(b, &data); err != nil || data.Version != 1 {
		return nil // ignore corrupt or unknown-version files
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range data.Counts {
		s.counts[k] += v
	}
	for k, v := range data.Failures {
		s.failures[k] += v
	}
	for k, v := range data.Quality {
		if v == nil {
			continue
		}
		parts := strings.SplitN(k, ":", 2)
		if len(parts) != 2 {
			continue
		}
		phaseInt, parseErr := strconv.Atoi(parts[0])
		if parseErr != nil {
			continue
		}
		qk := qualityKey{BuildPhase(phaseInt), parts[1]}
		if s.quality[qk] == nil {
			s.quality[qk] = &providerQuality{}
		}
		s.quality[qk].Successes += v.Successes
		s.quality[qk].Failures += v.Failures
	}
	return nil
}
