// candidates.go — Candidate selection and build strategy resolution.
package router

import (
	"fmt"
	"math"
)

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
		modelID := getModelID(provName, entry.Tier)

		access := r.getAccessType(provName)

		// Prior bias encodes static table preference:
		// position 0 (primary) → bias 2.0, position 1 → 1.0, position 2+ → 0.0.
		// This seeds the Beta distribution so observed quality can gradually override
		// the static order rather than starting from a blank slate.
		priorBias := 0.0
		if pos, ok := staticOrder[provName]; ok {
			priorBias = math.Max(0.0, float64(2-pos))
		}

		candidates = append(candidates, candidate{
			name:           provName,
			modelID:        modelID,
			access:         access,
			order:          i,
			useCount:       r.usage.Count(provName),
			failureRate:    r.usage.FailureRate(provName),
			requests:       r.usage.Count(provName) + r.usage.FailureCount(provName),
			qualitySamples: r.usage.QualitySampleCount(phase, provName),
			thompsonScore:  r.usage.ThompsonSample(phase, provName, priorBias),
		})
	}

	sortCandidatesForMode(candidates, r.effectiveMode())
	return candidates
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
		access := r.getAccessType(provName)
		modelID := getModelID(provName, bs.Tier)

		priorBias := 0.0
		if pos, ok := staticOrder[provName]; ok {
			priorBias = math.Max(0.0, float64(2-pos))
		}

		candidates = append(candidates, candidate{
			name:           provName,
			modelID:        modelID,
			access:         access,
			order:          i,
			useCount:       r.usage.Count(provName),
			failureRate:    r.usage.FailureRate(provName),
			requests:       r.usage.Count(provName) + r.usage.FailureCount(provName),
			qualitySamples: r.usage.QualitySampleCount(phase, provName),
			thompsonScore:  r.usage.ThompsonSample(phase, provName, priorBias),
		})
	}

	sortCandidatesForMode(candidates, mode)
	return bs, candidates, nil
}

func (r *Router) BuildProviderFor(phase BuildPhase) string {
	bs, ok := getBuildStrategy(r.effectiveMode(), phase)
	if !ok {
		return ""
	}
	return bs.Primary
}

func (r *Router) buildStrategyForPhase(phase BuildPhase) (BuildStrategy, error) {
	bs, ok := getBuildStrategy(r.effectiveMode(), phase)
	if !ok {
		return BuildStrategy{}, fmt.Errorf("unknown build strategy for mode %d phase %d", r.effectiveMode(), phase)
	}
	return bs, nil
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

func (r *Router) modeEntry(task TaskType) (strategyEntry, error) {
	entry, ok := getStrategyEntry(r.effectiveMode(), task)
	if !ok {
		return strategyEntry{}, fmt.Errorf("unknown usage mode: %d", r.effectiveMode())
	}
	return entry, nil
}
