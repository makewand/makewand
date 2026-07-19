package router

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed defaults.json
var defaultsJSON []byte

// strategyEntry defines the tier and provider preference order for a task.
type strategyEntry struct {
	Tier      ModelTier
	Providers []string
}

// costEntry holds per-million-token pricing.
type costEntry struct {
	Input  float64 // $/MTok input
	Output float64 // $/MTok output
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

// powerEnsemble defines which providers run in parallel (generators) and which
// single provider evaluates the outputs and picks the best one (judge) in Power mode.
type powerEnsemble struct {
	Generators []string // called concurrently; best ≥ 1 must succeed
	Judge      string   // selects the winner; must differ from Generators
}

// DefaultContextBudget is the fallback when no budget is configured.
const DefaultContextBudget = 3000

// strategyTables holds one snapshot of every mutable routing table:
// model assignment, pricing, per-mode strategies, build strategies,
// power ensembles, and context budgets.
//
// Each Router owns its own instance (deep-copied from the immutable package
// defaults at construction), so user overrides and hot-reloads on one Router
// never leak into another.
//
// mu guards the map fields. Updates are copy-on-write: applyOverrides merges
// into a private deep copy, validates it, and swaps the top-level maps in under
// the write lock. The maps reachable from a live snapshot are therefore never
// mutated in place, and accessors may return inner values without copying.
type strategyTables struct {
	mu              sync.RWMutex
	models          map[string]map[ModelTier]string            // (provider, tier) → model ID
	costs           map[string]costEntry                       // model ID → pricing
	strategies      map[UsageMode]map[TaskType]strategyEntry   // (mode, task) → strategy
	buildStrategies map[UsageMode]map[BuildPhase]BuildStrategy // (mode, phase) → build strategy
	powerEnsembles  map[BuildPhase]powerEnsemble               // phase → ensemble setup (Judge ∉ Generators)
	contextBudgets  map[string]map[ModelTier]int               // (provider, tier) → context budget in chars
}

// rawDefaults is the JSON schema for defaults.json and user overrides.
//
// Struct fields are pointers (or nil-able slices) so a field-level merge can
// tell an absent field apart from an explicit zero value: an absent field keeps
// the current/default value, while an explicit value (including 0 or "") is
// applied. Decoding is strict (DisallowUnknownFields) so typos surface as
// errors instead of being silently ignored.
type rawDefaults struct {
	Models          map[string]map[string]string      `json:"models"`
	Costs           map[string]rawCost                `json:"costs"`
	Strategies      map[string]map[string]rawStrategy `json:"strategies"`
	BuildStrategies map[string]map[string]rawBuild    `json:"build_strategies"`
	ContextBudgets  map[string]map[string]int         `json:"context_budgets"`
	PowerEnsemble   map[string]rawEnsemble            `json:"power_ensemble"`
}

// rawCost is a partial pricing override. A nil field is absent (keeps the
// existing value); a non-nil field — including an explicit 0 — is applied.
type rawCost struct {
	Input  *float64 `json:"input"`
	Output *float64 `json:"output"`
}

type rawStrategy struct {
	Tier      *string  `json:"tier"`
	Providers []string `json:"providers"`
}

type rawBuild struct {
	Tier      *string  `json:"tier"`
	Primary   *string  `json:"primary"`
	Fallbacks []string `json:"fallbacks"`
}

type rawEnsemble struct {
	Generators []string `json:"generators"`
	Judge      *string  `json:"judge"`
}

// decodeRawDefaults strictly decodes defaults/override JSON. Unknown fields,
// trailing data, and a top-level JSON null are rejected so malformed overrides
// fail loudly instead of silently decoding to an empty override.
func decodeRawDefaults(data []byte) (rawDefaults, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// Decode into a pointer so a bare top-level null is distinguishable from an
	// object: JSON null leaves the pointer nil, whereas an object allocates one.
	// A null would otherwise decode into the zero rawDefaults (all fields absent)
	// and be silently accepted as "no overrides".
	var raw *rawDefaults
	if err := dec.Decode(&raw); err != nil {
		return rawDefaults{}, err
	}
	if raw == nil {
		return rawDefaults{}, fmt.Errorf("routing override must be a JSON object, got null")
	}
	// The object must be the only top-level value. dec.More() is unreliable here
	// — it accepts stray closing tokens such as "{} }" or "{} ]" — so require a
	// second decode to hit io.EOF instead.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			return rawDefaults{}, fmt.Errorf("unexpected trailing data after JSON object")
		}
		return rawDefaults{}, fmt.Errorf("unexpected trailing data after JSON object: %w", err)
	}
	return *raw, nil
}

var (
	// baseTables holds the parsed embedded strategy defaults. It is never
	// mutated after init; Router instances start from a deep copy of it
	// (see copyDefaultTables).
	baseTables *strategyTables

	// defaultTables backs only the deprecated package-level helpers
	// (LoadUserOverrides, ContextBudgetForProvider, ContextBudgetForMode,
	// EstimateCost). No Router reads it: NewRouterFromConfig deep-copies the
	// defaults, and struct-literal Routers lazily adopt their own copy in
	// routingTables(), so package-level LoadUserOverrides never affects routing.
	defaultTables *strategyTables

	// initErr records any failure from loading embedded strategy defaults.
	// Returned from NewRouterFromConfig so callers get an error instead of a panic.
	initErr error
)

func init() {
	tables, err := parseStrategyTables(defaultsJSON)
	if err != nil {
		initErr = fmt.Errorf("strategy defaults: %w", err)
		tables = newStrategyTables()
	} else if err := tables.validate(); err != nil {
		initErr = fmt.Errorf("strategy table validation: %w", err)
	}
	baseTables = tables
	defaultTables = tables.copy()
}

func newStrategyTables() *strategyTables {
	return &strategyTables{
		models:          make(map[string]map[ModelTier]string),
		costs:           make(map[string]costEntry),
		strategies:      make(map[UsageMode]map[TaskType]strategyEntry),
		buildStrategies: make(map[UsageMode]map[BuildPhase]BuildStrategy),
		powerEnsembles:  make(map[BuildPhase]powerEnsemble),
		contextBudgets:  make(map[string]map[ModelTier]int),
	}
}

// copyDefaultTables returns a deep copy of the embedded package defaults.
// Every Router built through NewRouterFromConfig starts from its own copy.
func copyDefaultTables() *strategyTables {
	return baseTables.copy()
}

func parseStrategyTables(data []byte) (*strategyTables, error) {
	raw, err := decodeRawDefaults(data)
	if err != nil {
		return nil, err
	}
	t := newStrategyTables()
	if err := t.mergeOverrides(raw); err != nil {
		return nil, err
	}
	return t, nil
}

// copy returns a deep copy of the current snapshot.
func (t *strategyTables) copy() *strategyTables {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.copyLocked()
}

func (t *strategyTables) copyLocked() *strategyTables {
	out := newStrategyTables()
	for prov, tiers := range t.models {
		m := make(map[ModelTier]string, len(tiers))
		for tier, modelID := range tiers {
			m[tier] = modelID
		}
		out.models[prov] = m
	}
	for id, entry := range t.costs {
		out.costs[id] = entry
	}
	for mode, tasks := range t.strategies {
		m := make(map[TaskType]strategyEntry, len(tasks))
		for task, entry := range tasks {
			m[task] = strategyEntry{
				Tier:      entry.Tier,
				Providers: append([]string(nil), entry.Providers...),
			}
		}
		out.strategies[mode] = m
	}
	for mode, phases := range t.buildStrategies {
		m := make(map[BuildPhase]BuildStrategy, len(phases))
		for phase, bs := range phases {
			m[phase] = BuildStrategy{
				Tier:      bs.Tier,
				Primary:   bs.Primary,
				Fallbacks: append([]string(nil), bs.Fallbacks...),
			}
		}
		out.buildStrategies[mode] = m
	}
	for phase, pe := range t.powerEnsembles {
		out.powerEnsembles[phase] = powerEnsemble{
			Generators: append([]string(nil), pe.Generators...),
			Judge:      pe.Judge,
		}
	}
	for prov, tiers := range t.contextBudgets {
		m := make(map[ModelTier]int, len(tiers))
		for tier, budget := range tiers {
			m[tier] = budget
		}
		out.contextBudgets[prov] = m
	}
	return out
}

// mergeOverrides deep-merges the raw override data into t at field granularity.
// Only the fields present in the override are applied; absent fields keep their
// current values, down to individual (provider, tier)/(mode, task) keys AND the
// individual fields of each entry (a partial cost or strategy keeps the other
// fields' defaults). Unknown tier/mode/task/phase strings are rejected.
// Callers must have exclusive access to t (it is only used on private copies).
func (t *strategyTables) mergeOverrides(raw rawDefaults) error {
	// Models: merge per (provider, tier) so overriding one tier keeps the rest.
	for prov, tiers := range raw.Models {
		m := t.models[prov]
		if m == nil {
			m = make(map[ModelTier]string, 3)
			t.models[prov] = m
		}
		for tierName, modelID := range tiers {
			tier, err := parseTierStrict(tierName)
			if err != nil {
				return fmt.Errorf("models[%s]: %w", prov, err)
			}
			m[tier] = modelID
		}
	}

	// Costs: field-level merge per model ID — a partial entry (e.g. only
	// "input") keeps the other field's existing value.
	for id, rc := range raw.Costs {
		entry := t.costs[id]
		if rc.Input != nil {
			entry.Input = *rc.Input
		}
		if rc.Output != nil {
			entry.Output = *rc.Output
		}
		t.costs[id] = entry
	}

	// Strategies: field-level merge per (mode, task). Overriding only the
	// providers keeps the existing tier, and vice versa.
	for modeName, tasks := range raw.Strategies {
		mode, err := parseModeStrict(modeName)
		if err != nil {
			return fmt.Errorf("strategies: %w", err)
		}
		m := t.strategies[mode]
		if m == nil {
			m = make(map[TaskType]strategyEntry, len(tasks))
			t.strategies[mode] = m
		}
		for taskName, rs := range tasks {
			task, err := parseTaskStrict(taskName)
			if err != nil {
				return fmt.Errorf("strategies[%s]: %w", modeName, err)
			}
			entry := m[task]
			if rs.Tier != nil {
				tier, err := parseTierStrict(*rs.Tier)
				if err != nil {
					return fmt.Errorf("strategies[%s][%s]: %w", modeName, taskName, err)
				}
				entry.Tier = tier
			}
			if rs.Providers != nil {
				entry.Providers = append([]string(nil), rs.Providers...)
			}
			m[task] = entry
		}
	}

	// Build strategies: field-level merge per (mode, phase).
	for modeName, phases := range raw.BuildStrategies {
		mode, err := parseModeStrict(modeName)
		if err != nil {
			return fmt.Errorf("build_strategies: %w", err)
		}
		m := t.buildStrategies[mode]
		if m == nil {
			m = make(map[BuildPhase]BuildStrategy, len(phases))
			t.buildStrategies[mode] = m
		}
		for phaseName, rb := range phases {
			phase, err := parsePhaseStrict(phaseName)
			if err != nil {
				return fmt.Errorf("build_strategies[%s]: %w", modeName, err)
			}
			entry := m[phase]
			if rb.Tier != nil {
				tier, err := parseTierStrict(*rb.Tier)
				if err != nil {
					return fmt.Errorf("build_strategies[%s][%s]: %w", modeName, phaseName, err)
				}
				entry.Tier = tier
			}
			if rb.Primary != nil {
				entry.Primary = *rb.Primary
			}
			if rb.Fallbacks != nil {
				entry.Fallbacks = append([]string(nil), rb.Fallbacks...)
			}
			m[phase] = entry
		}
	}

	// Context budgets: merge per (provider, tier).
	for prov, tiers := range raw.ContextBudgets {
		m := t.contextBudgets[prov]
		if m == nil {
			m = make(map[ModelTier]int, len(tiers))
			t.contextBudgets[prov] = m
		}
		for tierName, budget := range tiers {
			tier, err := parseTierStrict(tierName)
			if err != nil {
				return fmt.Errorf("context_budgets[%s]: %w", prov, err)
			}
			m[tier] = budget
		}
	}

	// Power ensemble: field-level merge per phase.
	for phaseName, re := range raw.PowerEnsemble {
		phase, err := parsePhaseStrict(phaseName)
		if err != nil {
			return fmt.Errorf("power_ensemble: %w", err)
		}
		entry := t.powerEnsembles[phase]
		if re.Generators != nil {
			entry.Generators = append([]string(nil), re.Generators...)
		}
		if re.Judge != nil {
			entry.Judge = *re.Judge
		}
		t.powerEnsembles[phase] = entry
	}

	return nil
}

// applyOverrides strictly parses override JSON, merges it onto a fresh copy of
// the immutable package defaults, validates the result, and atomically swaps it
// in under the write lock. On any parse or validation error the current
// snapshot is kept unchanged.
//
// Recomputing from the defaults (rather than merging onto the live snapshot) is
// deliberate: it makes hot-reload idempotent, so deleting an override from
// routing.json reverts to the default value instead of leaving the previous
// override in place.
func (t *strategyTables) applyOverrides(data []byte) error {
	raw, err := decodeRawDefaults(data)
	if err != nil {
		return err
	}

	candidate := baseTables.copy()
	if err := candidate.mergeOverrides(raw); err != nil {
		return err
	}
	if err := candidate.validate(); err != nil {
		return err
	}

	t.replace(candidate)
	return nil
}

// resetToDefaults atomically replaces the live snapshot with a fresh copy of the
// immutable package defaults, discarding any applied overrides. The hot-reload
// watcher calls this when routing.json is deleted so removing the whole override
// file reverts to defaults instead of leaving the last overrides in place.
func (t *strategyTables) resetToDefaults() {
	t.replace(baseTables.copy())
}

// replace atomically swaps in the top-level maps from candidate under the write
// lock. candidate must be a private snapshot the caller no longer mutates; the
// maps reachable from a live snapshot are never mutated in place, so accessors
// may return inner values without copying.
func (t *strategyTables) replace(candidate *strategyTables) {
	t.mu.Lock()
	t.models = candidate.models
	t.costs = candidate.costs
	t.strategies = candidate.strategies
	t.buildStrategies = candidate.buildStrategies
	t.powerEnsembles = candidate.powerEnsembles
	t.contextBudgets = candidate.contextBudgets
	t.mu.Unlock()
}

// loadUserOverrides loads user-customized routing tables from configDir/routing.json.
// Missing file is not an error. Fields present in the override file are
// deep-merged over the current snapshot; absent fields keep their values.
// Invalid overrides leave the snapshot unchanged and return the error.
func (t *strategyTables) loadUserOverrides(configDir string) error {
	path := filepath.Join(configDir, "routing.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := t.applyOverrides(data); err != nil {
		return fmt.Errorf("routing.json: %w", err)
	}
	return nil
}

// Table accessor helpers — acquire t.mu.RLock for safe concurrent reads.

func (t *strategyTables) modelID(provider string, tier ModelTier) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if models, ok := t.models[provider]; ok {
		return models[tier]
	}
	if base := baseProviderName(provider); base != provider {
		if models, ok := t.models[base]; ok {
			return models[tier]
		}
	}
	return ""
}

func baseProviderName(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return strings.TrimSuffix(provider, "-api")
}

func apiFallbackName(provider string) string {
	provider = baseProviderName(provider)
	if provider == "" || strings.HasSuffix(provider, "-api") {
		return ""
	}
	return provider + "-api"
}

func (t *strategyTables) strategyFor(mode UsageMode, task TaskType) (strategyEntry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	tasks, ok := t.strategies[mode]
	if !ok {
		return strategyEntry{}, false
	}
	entry, ok := tasks[task]
	if !ok {
		// Fall back to TaskCode within the lock.
		entry, ok = tasks[TaskCode]
	}
	return entry, ok
}

func (t *strategyTables) buildStrategy(mode UsageMode, phase BuildPhase) (BuildStrategy, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	phases, ok := t.buildStrategies[mode]
	if !ok {
		return BuildStrategy{}, false
	}
	bs, ok := phases[phase]
	return bs, ok
}

func (t *strategyTables) powerEnsembleFor(phase BuildPhase) (powerEnsemble, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	pe, ok := t.powerEnsembles[phase]
	return pe, ok
}

func (t *strategyTables) costFor(modelID string) (costEntry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.costs[modelID]
	return e, ok
}

// modelsFor returns the tier→model map for a provider. The returned map is a
// live snapshot member and must not be mutated; snapshots are copy-on-write,
// so it is safe to read after the lock is released.
func (t *strategyTables) modelsFor(provider string) (map[ModelTier]string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	m, ok := t.models[provider]
	return m, ok
}

// parseTierStrict maps a tier name to its enum, rejecting unknown values
// instead of silently defaulting to TierCheap.
func parseTierStrict(s string) (ModelTier, error) {
	switch s {
	case "cheap":
		return TierCheap, nil
	case "mid":
		return TierMid, nil
	case "premium":
		return TierPremium, nil
	default:
		return 0, fmt.Errorf("unknown tier %q (want cheap|mid|premium)", s)
	}
}

// parseModeStrict maps a mode name to its enum, rejecting unknown values.
func parseModeStrict(s string) (UsageMode, error) {
	if m, ok := ParseUsageMode(s); ok {
		return m, nil
	}
	return 0, fmt.Errorf("unknown mode %q (want fast|balanced|power)", s)
}

// parseTaskStrict maps a task name to its enum, rejecting unknown values
// instead of silently defaulting to TaskAnalyze.
func parseTaskStrict(s string) (TaskType, error) {
	switch s {
	case "analyze":
		return TaskAnalyze, nil
	case "explain":
		return TaskExplain, nil
	case "code":
		return TaskCode, nil
	case "fix":
		return TaskFix, nil
	case "review":
		return TaskReview, nil
	default:
		return 0, fmt.Errorf("unknown task %q (want analyze|explain|code|fix|review)", s)
	}
}

// parsePhaseStrict maps a build-phase name to its enum, rejecting unknown values
// instead of silently defaulting to PhasePlan.
func parsePhaseStrict(s string) (BuildPhase, error) {
	switch s {
	case "plan":
		return PhasePlan, nil
	case "code":
		return PhaseCode, nil
	case "review":
		return PhaseReview, nil
	case "fix":
		return PhaseFix, nil
	default:
		return 0, fmt.Errorf("unknown phase %q (want plan|code|review|fix)", s)
	}
}

// validate checks internal consistency of the strategy and cost tables.
func (t *strategyTables) validate() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.validateLocked()
}

// validateLocked assumes the caller has exclusive or read access to t.
func (t *strategyTables) validateLocked() error {
	// 1. Every strategy entry must list at least one provider, and every
	//    provider must have a models entry.
	for mode, tasks := range t.strategies {
		for task, entry := range tasks {
			if len(entry.Providers) == 0 {
				return fmt.Errorf("strategyTable[%d][%d]: providers list is empty", mode, task)
			}
			for _, prov := range entry.Providers {
				if _, ok := t.models[prov]; !ok {
					return fmt.Errorf("strategyTable[%d][%d]: provider %q not in modelTable", mode, task, prov)
				}
			}
		}
	}

	// 2. Every provider in buildStrategies must have a models entry.
	for mode, phases := range t.buildStrategies {
		for phase, bs := range phases {
			if _, ok := t.models[bs.Primary]; !ok {
				return fmt.Errorf("buildStrategyTable[%d][%d].Primary %q not in modelTable", mode, phase, bs.Primary)
			}
			for _, fb := range bs.Fallbacks {
				if _, ok := t.models[fb]; !ok {
					return fmt.Errorf("buildStrategyTable[%d][%d] fallback %q not in modelTable", mode, phase, fb)
				}
			}
		}
	}

	// 3. Every model ID in models must have a costs entry.
	for prov, tiers := range t.models {
		for tier, modelID := range tiers {
			if modelID == "" {
				continue
			}
			if _, ok := t.costs[modelID]; !ok {
				return fmt.Errorf("modelTable[%s][%d]: model %q not in costTable", prov, tier, modelID)
			}
		}
	}

	// 4. buildStrategies Primary must not appear in its own Fallbacks list.
	for mode, phases := range t.buildStrategies {
		for phase, bs := range phases {
			for _, fb := range bs.Fallbacks {
				if fb == bs.Primary {
					return fmt.Errorf("buildStrategyTable[%d][%d]: Primary %q appears in Fallbacks", mode, phase, bs.Primary)
				}
			}
		}
	}

	// 5. powerEnsembles: all providers must exist in models; Judge ∉ Generators.
	for phase, pe := range t.powerEnsembles {
		for _, g := range pe.Generators {
			if _, ok := t.models[g]; !ok {
				return fmt.Errorf("powerEnsembleTable[%d] generator %q not in modelTable", phase, g)
			}
			if g == pe.Judge {
				return fmt.Errorf("powerEnsembleTable[%d]: Judge %q must not appear in Generators", phase, g)
			}
		}
		if _, ok := t.models[pe.Judge]; !ok {
			return fmt.Errorf("powerEnsembleTable[%d] judge %q not in modelTable", phase, pe.Judge)
		}
	}

	return nil
}

func (t *strategyTables) contextBudget(provider string, tier ModelTier) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.contextBudgetLocked(provider, tier)
}

func (t *strategyTables) contextBudgetLocked(provider string, tier ModelTier) int {
	if tiers, ok := t.contextBudgets[provider]; ok {
		if budget, ok := tiers[tier]; ok {
			return budget
		}
	}
	if base := baseProviderName(provider); base != provider {
		if tiers, ok := t.contextBudgets[base]; ok {
			if budget, ok := tiers[tier]; ok {
				return budget
			}
		}
	}
	return DefaultContextBudget
}

func (t *strategyTables) contextBudgetForMode(mode UsageMode, task TaskType) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if tasks, ok := t.strategies[mode]; ok {
		if entry, ok := tasks[task]; ok {
			if len(entry.Providers) > 0 {
				return t.contextBudgetLocked(entry.Providers[0], entry.Tier)
			}
		}
	}
	return DefaultContextBudget
}

// Deprecated package-level wrappers — they operate on the package-level default
// tables only. Router instances keep their own tables and are unaffected.

// LoadUserOverrides loads user-customized routing tables from configDir/routing.json
// into the package-level default tables. Missing file is not an error. Fields
// present in the override file are deep-merged over the defaults; absent fields
// keep their default values.
//
// Deprecated: this only affects the package-level helpers (ContextBudgetForProvider,
// ContextBudgetForMode, EstimateCost). Use RouterConfig.ConfigDir or
// (*Router).LoadUserOverrides for per-instance overrides.
func LoadUserOverrides(configDir string) error {
	return defaultTables.loadUserOverrides(configDir)
}

// ContextBudgetForProvider returns the context budget (in chars) for a given
// provider and tier, read from the package-level default tables.
// Returns DefaultContextBudget when no entry is configured.
func ContextBudgetForProvider(provider string, tier ModelTier) int {
	return defaultTables.contextBudget(provider, tier)
}

// ContextBudgetForMode returns the context budget (in chars) for the first
// provider in the strategy table entry for the given (mode, task) combination,
// read from the package-level default tables.
// Returns DefaultContextBudget when no matching strategy entry exists.
func ContextBudgetForMode(mode UsageMode, task TaskType) int {
	return defaultTables.contextBudgetForMode(mode, task)
}
