// candidates.go — Candidate selection and build strategy resolution.
package router

import (
	"fmt"
	"math"
	"strings"
)

func expandProviderPreference(preferred []string, registered []string, excluded map[string]bool) []string {
	out := make([]string, 0, len(preferred)+len(registered))
	seen := make(map[string]bool, len(preferred)+len(registered))
	registeredSet := make(map[string]struct{}, len(registered))
	for _, name := range registered {
		registeredSet[name] = struct{}{}
	}

	appendName := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if excluded != nil && excluded[name] {
			return
		}
		if _, ok := registeredSet[name]; !ok {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	appendWithFallback := func(name string) {
		appendName(name)
		if alias := apiFallbackName(name); alias != "" {
			appendName(alias)
		}
	}

	for _, name := range preferred {
		appendWithFallback(name)
	}
	for _, name := range registered {
		appendWithFallback(name)
	}
	return out
}

func (r *Router) legacyFallbackCandidates(primaryName string) []fallbackCandidate {
	preferred := make([]string, 0, len(fallbackOrder)+1)
	if alias := apiFallbackName(primaryName); alias != "" {
		preferred = append(preferred, alias)
	}
	for _, name := range fallbackOrder {
		if name == primaryName {
			continue
		}
		preferred = append(preferred, name)
	}
	ordered := expandProviderPreference(preferred, r.registeredProviderNames(), map[string]bool{primaryName: true})
	out := make([]fallbackCandidate, 0, len(ordered))
	for _, name := range ordered {
		out = append(out, fallbackCandidate{name: name, modelID: r.legacyModelID(name)})
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
		modelID := r.routingTables().modelID(provName, entry.Tier)

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
			quotaBand:      r.quotaBandFor(provName),
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
		modelID := r.routingTables().modelID(provName, bs.Tier)

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
			quotaBand:      r.quotaBandFor(provName),
		})
	}

	sortCandidatesForMode(candidates, mode)
	return bs, candidates, nil
}

func (r *Router) BuildProviderFor(phase BuildPhase) string {
	if name, _, ok := r.remoteOnlyProvider(); ok {
		return name
	}
	bs, ok := r.routingTables().buildStrategy(r.effectiveMode(), phase)
	if !ok {
		return ""
	}
	return bs.Primary
}

func (r *Router) buildStrategyForPhase(phase BuildPhase) (BuildStrategy, error) {
	bs, ok := r.routingTables().buildStrategy(r.effectiveMode(), phase)
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
	if name, _, ok := r.remoteOnlyProvider(); ok {
		return name
	}
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

// BuildProvidersForAdaptive returns the adaptive provider order for a build phase.
// The returned names are filtered by availability, circuit state, and the caller's
// exclusion list. When limit > 0, at most limit providers are returned.
func (r *Router) BuildProvidersForAdaptive(phase BuildPhase, limit int, exclude ...string) []string {
	if name, _, ok := r.remoteOnlyProvider(); ok {
		if len(exclude) > 0 {
			for _, excluded := range exclude {
				if strings.EqualFold(strings.TrimSpace(excluded), name) {
					return nil
				}
			}
		}
		return []string{name}
	}

	excluded := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		excluded[name] = true
	}

	_, candidates, err := r.buildPhaseCandidates(phase, excluded)
	if err != nil {
		return nil
	}

	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if blocked, _ := r.isCircuitOpen(c.name); blocked {
			continue
		}
		if !r.isBuildProviderAvailable(c.name, c.modelID) {
			continue
		}
		out = append(out, c.name)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (r *Router) modeEntry(task TaskType) (strategyEntry, error) {
	entry, ok := r.routingTables().strategyFor(r.effectiveMode(), task)
	if !ok {
		return strategyEntry{}, fmt.Errorf("unknown usage mode: %d", r.effectiveMode())
	}
	return entry, nil
}
