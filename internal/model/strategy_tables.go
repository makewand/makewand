package model

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed defaults.json
var defaultsJSON []byte

// modelTable maps (provider, tier) → model ID.
var modelTable map[string]map[ModelTier]string

// strategyEntry defines the tier and provider preference order for a task.
type strategyEntry struct {
	Tier      ModelTier
	Providers []string
}

// strategyTable maps (UsageMode, TaskType) → strategyEntry.
var strategyTable map[UsageMode]map[TaskType]strategyEntry

// costEntry holds per-million-token pricing.
type costEntry struct {
	Input  float64 // $/MTok input
	Output float64 // $/MTok output
}

// costTable maps model ID → pricing.
var costTable map[string]costEntry

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
var buildStrategyTable map[UsageMode]map[BuildPhase]BuildStrategy

// powerEnsemble defines which providers run in parallel (generators) and which
// single provider evaluates the outputs and picks the best one (judge) in Power mode.
type powerEnsemble struct {
	Generators []string // called concurrently; best ≥ 1 must succeed
	Judge      string   // selects the winner; must differ from Generators
}

// powerEnsembleTable maps BuildPhase → ensemble setup for Power mode.
// Design rule: Judge ∉ Generators so evaluation is always cross-model.
var powerEnsembleTable map[BuildPhase]powerEnsemble

// rawDefaults is the JSON schema for defaults.json and user overrides.
type rawDefaults struct {
	Models          map[string]map[string]string     `json:"models"`
	Costs           map[string]costEntry             `json:"costs"`
	Strategies      map[string]map[string]rawStrategy `json:"strategies"`
	BuildStrategies map[string]map[string]rawBuild   `json:"build_strategies"`
	PowerEnsemble   map[string]rawEnsemble           `json:"power_ensemble"`
}

type rawStrategy struct {
	Tier      string   `json:"tier"`
	Providers []string `json:"providers"`
}

type rawBuild struct {
	Tier      string   `json:"tier"`
	Primary   string   `json:"primary"`
	Fallbacks []string `json:"fallbacks"`
}

type rawEnsemble struct {
	Generators []string `json:"generators"`
	Judge      string   `json:"judge"`
}

func init() {
	if err := loadDefaults(defaultsJSON); err != nil {
		panic("strategy defaults: " + err.Error())
	}
	if err := validateStrategyTables(); err != nil {
		panic("strategy table validation: " + err.Error())
	}
}

// LoadUserOverrides loads user-customized routing tables from configDir/routing.json.
// Missing file is not an error. Fields present in the override file replace the
// corresponding defaults; absent fields keep their default values.
func LoadUserOverrides(configDir string) error {
	path := filepath.Join(configDir, "routing.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := loadDefaults(data); err != nil {
		return fmt.Errorf("routing.json: %w", err)
	}
	return validateStrategyTables()
}

func loadDefaults(data []byte) error {
	var raw rawDefaults
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Models
	if raw.Models != nil {
		modelTable = make(map[string]map[ModelTier]string, len(raw.Models))
		for prov, tiers := range raw.Models {
			m := make(map[ModelTier]string, 3)
			for tierName, modelID := range tiers {
				m[parseTier(tierName)] = modelID
			}
			modelTable[prov] = m
		}
	}

	// Costs
	if raw.Costs != nil {
		costTable = make(map[string]costEntry, len(raw.Costs))
		for id, entry := range raw.Costs {
			costTable[id] = entry
		}
	}

	// Strategies
	if raw.Strategies != nil {
		strategyTable = make(map[UsageMode]map[TaskType]strategyEntry, len(raw.Strategies))
		for modeName, tasks := range raw.Strategies {
			mode := parseMode(modeName)
			m := make(map[TaskType]strategyEntry, len(tasks))
			for taskName, rs := range tasks {
				m[parseTask(taskName)] = strategyEntry{
					Tier:      parseTier(rs.Tier),
					Providers: rs.Providers,
				}
			}
			strategyTable[mode] = m
		}
	}

	// Build strategies
	if raw.BuildStrategies != nil {
		buildStrategyTable = make(map[UsageMode]map[BuildPhase]BuildStrategy, len(raw.BuildStrategies))
		for modeName, phases := range raw.BuildStrategies {
			mode := parseMode(modeName)
			m := make(map[BuildPhase]BuildStrategy, len(phases))
			for phaseName, rb := range phases {
				m[parsePhase(phaseName)] = BuildStrategy{
					Tier:      parseTier(rb.Tier),
					Primary:   rb.Primary,
					Fallbacks: rb.Fallbacks,
				}
			}
			buildStrategyTable[mode] = m
		}
	}

	// Power ensemble
	if raw.PowerEnsemble != nil {
		powerEnsembleTable = make(map[BuildPhase]powerEnsemble, len(raw.PowerEnsemble))
		for phaseName, re := range raw.PowerEnsemble {
			powerEnsembleTable[parsePhase(phaseName)] = powerEnsemble{
				Generators: re.Generators,
				Judge:      re.Judge,
			}
		}
	}

	return nil
}

func parseTier(s string) ModelTier {
	switch s {
	case "mid":
		return TierMid
	case "premium":
		return TierPremium
	default:
		return TierCheap
	}
}

func parseMode(s string) UsageMode {
	m, _ := ParseUsageMode(s)
	return m
}

func parseTask(s string) TaskType {
	switch s {
	case "analyze":
		return TaskAnalyze
	case "explain":
		return TaskExplain
	case "code":
		return TaskCode
	case "fix":
		return TaskFix
	case "review":
		return TaskReview
	default:
		return TaskAnalyze
	}
}

func parsePhase(s string) BuildPhase {
	switch s {
	case "code":
		return PhaseCode
	case "review":
		return PhaseReview
	case "fix":
		return PhaseFix
	default:
		return PhasePlan
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
