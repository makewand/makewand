package model

import (
	"encoding/json"
	"fmt"
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
	ModeFree     UsageMode = iota // Free: only free/local/subscription providers
	ModeEconomy                   // Economy: cheap tier, prefer free providers
	ModeBalanced                  // Balanced: mid tier, good quality/cost ratio
	ModePower                     // Power: premium tier, best quality
)

// AccessType describes how a provider is accessed.
type AccessType int

const (
	AccessFree         AccessType = iota // Free tier (e.g. Gemini Flash)
	AccessLocal                          // Local model (e.g. Ollama)
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

// modelTable maps (provider, tier) → model ID.
// For CLI providers, model ID is ignored (CLI uses subscription default),
// but we still need entries for the routing logic.
var modelTable = map[string]map[ModelTier]string{
	"claude": {
		TierCheap:   "claude-haiku-4-5-20251001",
		TierMid:     "claude-sonnet-4-20250514",
		TierPremium: "claude-opus-4-20250514",
	},
	"codex": {
		TierCheap:   "codex-cli", // subscription CLI — cost is zero
		TierMid:     "codex-cli",
		TierPremium: "codex-cli",
	},
	"openai": {
		TierCheap:   "gpt-4o-mini",
		TierMid:     "gpt-4o",
		TierPremium: "gpt-4o",
	},
	"gemini": {
		TierCheap:   "gemini-2.5-flash",
		TierMid:     "gemini-2.5-flash",
		TierPremium: "gemini-2.5-pro",
	},
	"ollama": {
		TierCheap:   "gemma3:4b",
		TierMid:     "gemma3:27b",
		TierPremium: "llama4:16x17b",
	},
}

// strategyEntry defines the tier and provider preference order for a task.
type strategyEntry struct {
	Tier      ModelTier
	Providers []string
}

// strategyTable maps (UsageMode, TaskType) → strategyEntry.
var strategyTable = map[UsageMode]map[TaskType]strategyEntry{
	ModeFree: {
		TaskAnalyze: {TierCheap, []string{"gemini", "ollama"}},
		TaskExplain: {TierCheap, []string{"gemini", "ollama"}},
		TaskCode:    {TierCheap, []string{"gemini", "ollama"}},
		TaskFix:     {TierCheap, []string{"gemini", "ollama"}},
		TaskReview:  {TierCheap, []string{"gemini", "ollama"}},
	},
	ModeEconomy: {
		TaskAnalyze: {TierCheap, []string{"gemini", "ollama", "claude", "codex", "openai"}},
		TaskExplain: {TierCheap, []string{"gemini", "ollama", "claude", "codex", "openai"}},
		TaskCode:    {TierCheap, []string{"gemini", "claude", "codex", "openai", "ollama"}},
		TaskFix:     {TierCheap, []string{"codex", "claude", "gemini", "openai", "ollama"}},
		TaskReview:  {TierCheap, []string{"gemini", "ollama", "claude", "codex", "openai"}},
	},
	ModeBalanced: {
		TaskAnalyze: {TierMid, []string{"gemini", "claude", "codex", "openai", "ollama"}},
		TaskExplain: {TierMid, []string{"gemini", "claude", "codex", "openai", "ollama"}},
		TaskCode:    {TierMid, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskFix:     {TierMid, []string{"codex", "claude", "openai", "gemini", "ollama"}},
		TaskReview:  {TierMid, []string{"gemini", "claude", "codex", "openai", "ollama"}},
	},
	ModePower: {
		TaskAnalyze: {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskExplain: {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskCode:    {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskFix:     {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
		TaskReview:  {TierPremium, []string{"claude", "codex", "openai", "gemini", "ollama"}},
	},
}

// costEntry holds per-million-token pricing.
type costEntry struct {
	Input  float64 // $/MTok input
	Output float64 // $/MTok output
}

// costTable maps model ID → pricing.
var costTable = map[string]costEntry{
	"claude-haiku-4-5-20251001": {0.25, 1.25},
	"claude-sonnet-4-20250514":  {3.0, 15.0},
	"claude-opus-4-20250514":    {15.0, 75.0},
	"gpt-4o-mini":               {0.15, 0.60},
	"gpt-4o":                    {2.50, 10.0},
	"gemini-2.5-flash":          {0, 0},
	"gemini-2.5-pro":            {1.25, 10.0},
	"codex-cli":                 {0, 0},
	"llama3.2":                  {0, 0},
	"gemma3:4b":                 {0, 0},
	"gemma3:27b":                {0, 0},
	"llama4:16x17b":             {0, 0},
}

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
	case "free":
		return ModeFree, true
	case "economy":
		return ModeEconomy, true
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
	case ModeFree:
		return "free"
	case ModeEconomy:
		return "economy"
	case ModeBalanced:
		return "balanced"
	case ModePower:
		return "power"
	default:
		return "unknown"
	}
}

// parseAccessType determines the AccessType from a config value and provider name.
// Defaults: gemini→Free, ollama→Local, others→API.
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
	// Default inference based on provider
	switch providerName {
	case "gemini":
		return AccessFree
	case "ollama":
		return AccessLocal
	default:
		return AccessAPI
	}
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
	failures map[string]int          // provider errors / timeouts
	quality  map[qualityKey]*providerQuality // Beta(α,β) per (phase, provider)
}

func newSessionUsage() *sessionUsage {
	return &sessionUsage{
		counts:   make(map[string]int),
		failures: make(map[string]int),
		quality:  make(map[qualityKey]*providerQuality),
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
	s.mu.Unlock()
	return betaSample(alpha, beta)
}

// betaSample draws a random variate from Beta(alpha, beta) via the Gamma relationship.
func betaSample(alpha, beta float64) float64 {
	x := gammaSample(alpha)
	y := gammaSample(beta)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaSample draws from Gamma(alpha, 1) using Marsaglia-Tsang's method (Go math/rand).
func gammaSample(alpha float64) float64 {
	if alpha < 1.0 {
		return gammaSample(1.0+alpha) * math.Pow(rand.Float64(), 1.0/alpha)
	}
	d := alpha - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		var x, v float64
		for {
			x = rand.NormFloat64()
			v = 1.0 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := rand.Float64()
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
	name          string
	modelID       string
	access        AccessType
	order         int     // original position in strategy table
	useCount      int     // session success count
	failureRate   float64 // fraction of requests that errored this session
	requests      int     // total requests (counts + failures) for min-sample gate
	thompsonScore float64 // sampled from Beta(α,β); higher → higher priority
}

// BuildPhase represents a phase in the multi-model build pipeline.
type BuildPhase int

const (
	PhasePlan   BuildPhase = iota // requirements analysis / planning
	PhaseCode                     // code generation
	PhaseReview                   // cross-model code review
	PhaseFix                      // error fixing
)

// BuildStrategy defines the provider preference for a build phase.
type BuildStrategy struct {
	Tier      ModelTier
	Primary   string   // preferred provider
	Fallbacks []string // fallback order
}

// buildStrategyTable maps (UsageMode, BuildPhase) → BuildStrategy.
// Review and Fix Primary must differ from Code — cross-model perspective is the core value.
var buildStrategyTable = map[UsageMode]map[BuildPhase]BuildStrategy{
	ModeFree: {
		PhasePlan:   {TierCheap, "gemini", []string{"ollama"}},
		PhaseCode:   {TierCheap, "gemini", []string{"ollama"}},
		PhaseReview: {TierCheap, "ollama", []string{"gemini"}},
		PhaseFix:    {TierCheap, "ollama", []string{"gemini"}},
	},
	ModeEconomy: {
		PhasePlan:   {TierCheap, "gemini", []string{"ollama", "claude", "codex"}},
		PhaseCode:   {TierCheap, "gemini", []string{"claude", "codex", "ollama"}},
		PhaseReview: {TierCheap, "claude", []string{"ollama", "gemini", "codex"}},
		PhaseFix:    {TierCheap, "codex", []string{"claude", "gemini", "ollama"}},
	},
	ModeBalanced: {
		PhasePlan:   {TierMid, "gemini", []string{"claude", "codex", "ollama"}},
		PhaseCode:   {TierMid, "claude", []string{"codex", "gemini", "ollama"}},
		PhaseReview: {TierMid, "gemini", []string{"codex", "ollama", "claude"}},
		PhaseFix:    {TierMid, "codex", []string{"claude", "gemini", "ollama"}},
	},
	ModePower: {
		PhasePlan:   {TierPremium, "gemini", []string{"claude", "codex", "ollama"}},
		PhaseCode:   {TierPremium, "claude", []string{"codex", "gemini", "ollama"}},
		PhaseReview: {TierPremium, "gemini", []string{"codex", "ollama", "claude"}},
		PhaseFix:    {TierPremium, "codex", []string{"claude", "gemini", "ollama"}},
	},
}

// minSamplesForExclusion is the minimum number of total requests (success + error)
// required before the hard >50% error-failure-rate exclusion rule applies.
// This prevents a single transient API error from permanently blacklisting a provider.
const minSamplesForExclusion = 5

// sortCandidates sorts candidates by:
//  1. Access type priority (Free/Subscription < Local < API)
//  2. Hard exclusion: >50% error-failure rate with ≥5 total requests
//  3. Thompson Sampling score (adaptive quality signal seeded by static table priors)
//  4. Session use count (load balance among equal-score candidates)
//  5. Original strategy table order as a stable tiebreaker
func sortCandidates(candidates []candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		pi, pj := accessPriority(candidates[i].access), accessPriority(candidates[j].access)
		if pi != pj {
			return pi < pj
		}
		// Hard-exclude providers with >50% error-failure rate, but only after
		// enough samples to avoid false-positives from transient errors.
		hi := candidates[i].failureRate > 0.5 && candidates[i].requests >= minSamplesForExclusion
		hj := candidates[j].failureRate > 0.5 && candidates[j].requests >= minSamplesForExclusion
		if hi != hj {
			return !hi
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

// powerEnsemble defines which providers run in parallel (generators) and which
// single provider evaluates the outputs and picks the best one (judge) in Power mode.
// Each phase has distinct generators and an independent judge.
type powerEnsemble struct {
	Generators []string // called concurrently; best ≥ 1 must succeed
	Judge      string   // selects the winner; must differ from Generators
}

// powerEnsembleTable maps BuildPhase → ensemble setup for Power mode.
// Design rule: Judge ∉ Generators so evaluation is always cross-model.
var powerEnsembleTable = map[BuildPhase]powerEnsemble{
	PhasePlan:   {[]string{"gemini", "claude"}, "codex"},
	PhaseCode:   {[]string{"claude", "codex"}, "gemini"},
	PhaseReview: {[]string{"gemini", "claude"}, "codex"},
	PhaseFix:    {[]string{"codex", "claude"}, "gemini"},
}

// init validates the strategy tables at startup so misconfiguration is caught early.
func init() {
	if err := validateStrategyTables(); err != nil {
		panic("strategy table validation: " + err.Error())
	}
}

// validateStrategyTables checks internal consistency of the strategy and cost tables.
func validateStrategyTables() error {
	// 1. Every provider in strategyTable must have a modelTable entry.
	for mode, tasks := range strategyTable {
		for task, entry := range tasks {
			for _, prov := range entry.Providers {
				if _, ok := modelTable[prov]; !ok {
					return fmt.Errorf("strategyTable[%d][%d]: provider %q not in modelTable", mode, task, prov)
				}
			}
		}
	}

	// 2. Every provider in buildStrategyTable must have a modelTable entry.
	for mode, phases := range buildStrategyTable {
		for phase, bs := range phases {
			if _, ok := modelTable[bs.Primary]; !ok {
				return fmt.Errorf("buildStrategyTable[%d][%d].Primary %q not in modelTable", mode, phase, bs.Primary)
			}
			for _, fb := range bs.Fallbacks {
				if _, ok := modelTable[fb]; !ok {
					return fmt.Errorf("buildStrategyTable[%d][%d] fallback %q not in modelTable", mode, phase, fb)
				}
			}
		}
	}

	// 3. Every model ID in modelTable must have a costTable entry.
	for prov, tiers := range modelTable {
		for tier, modelID := range tiers {
			if modelID == "" {
				continue
			}
			if _, ok := costTable[modelID]; !ok {
				return fmt.Errorf("modelTable[%s][%d]: model %q not in costTable", prov, tier, modelID)
			}
		}
	}

	// 4. buildStrategyTable Primary must not appear in its own Fallbacks list.
	for mode, phases := range buildStrategyTable {
		for phase, bs := range phases {
			for _, fb := range bs.Fallbacks {
				if fb == bs.Primary {
					return fmt.Errorf("buildStrategyTable[%d][%d]: Primary %q appears in Fallbacks", mode, phase, bs.Primary)
				}
			}
		}
	}

	// 5. powerEnsembleTable: all providers must exist in modelTable; Judge ∉ Generators.
	for phase, pe := range powerEnsembleTable {
		for _, g := range pe.Generators {
			if _, ok := modelTable[g]; !ok {
				return fmt.Errorf("powerEnsembleTable[%d] generator %q not in modelTable", phase, g)
			}
			if g == pe.Judge {
				return fmt.Errorf("powerEnsembleTable[%d]: Judge %q must not appear in Generators", phase, g)
			}
		}
		if _, ok := modelTable[pe.Judge]; !ok {
			return fmt.Errorf("powerEnsembleTable[%d] judge %q not in modelTable", phase, pe.Judge)
		}
	}

	return nil
}

// statsFile is the filename for cross-session routing quality statistics.
const statsFile = "routing_stats.json"

// persistedStats is the JSON schema for routing_stats.json.
type persistedStats struct {
	Version  int                        `json:"version"`
	Counts   map[string]int             `json:"counts"`
	Failures map[string]int             `json:"failures"`
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
