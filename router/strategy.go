package router

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// EstimateCost returns the estimated cost in USD for the given model and token counts.
func EstimateCost(modelID string, inputTokens, outputTokens int) float64 {
	entry, ok := costTable[modelID]
	if !ok {
		return 0
	}
	return float64(inputTokens)/1_000_000*entry.Input + float64(outputTokens)/1_000_000*entry.Output
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

// parseAccessType determines the AccessType from a config value and provider name.
// Defaults: all built-in providers default to Subscription (CLI tools preferred).
func parseAccessType(configValue, providerName string) AccessType {
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
// Lower is preferred: Free/Subscription(0) < Local(1) < API(2).
// Subscription is treated as "free" because the user already paid a flat monthly fee.
func accessPriority(at AccessType) int {
	switch at {
	case AccessFree, AccessSubscription:
		return 0
	case AccessLocal:
		return 1
	case AccessAPI:
		return 2
	default:
		return 3
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
	mu       sync.Mutex
	counts   map[string]int
	failures map[string]int                  // provider errors / timeouts
	quality  map[qualityKey]*providerQuality // Beta(α,β) per (phase, provider)
	rng      *rand.Rand                      // injectable random source for deterministic tests
}

func newSessionUsage() *sessionUsage {
	return &sessionUsage{
		counts:   make(map[string]int),
		failures: make(map[string]int),
		quality:  make(map[qualityKey]*providerQuality),
		rng:      rand.New(rand.NewSource(rand.Int63())),
	}
}

func (s *sessionUsage) Increment(provider string) {
	s.mu.Lock()
	s.counts[provider]++
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
}

// ThompsonSample draws a score from Beta(α, β) for (phase, provider).
//
// priorBias seeds α with virtual successes proportional to the provider's position
// in the static strategy table (position 0 → bias 2.0, position 1 → 1.0, 2+ → 0.0),
// so the static table acts as a Bayesian prior that gets overridden by observed quality.
func (s *sessionUsage) ThompsonSample(phase BuildPhase, provider string, priorBias float64) float64 {
	s.mu.Lock()
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
	rng := s.rng
	s.mu.Unlock()
	return betaSample(rng, alpha, beta)
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
	order          int     // original position in strategy table
	useCount       int     // session success count
	failureRate    float64 // fraction of requests that errored this session
	requests       int     // total requests (counts + failures) for min-sample gate
	qualitySamples int     // total quality outcomes (success + failure) for this phase
	thompsonScore  float64 // sampled from Beta(α,β); higher → higher priority
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

// Save writes the session's routing statistics to configDir/routing_stats.json.
// Existing data is replaced; the file is written atomically via a temp file.
func (s *sessionUsage) Save(configDir string) error {
	s.mu.Lock()
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
	tmpPath := filepath.Join(configDir, statsFile+".tmp")
	if err := os.WriteFile(tmpPath, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(configDir, statsFile))
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
