// ensemble.go — Power mode ensemble orchestration.
package router

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EnsembleResult holds one provider's response in a parallel ensemble run.
type EnsembleResult struct {
	Provider string
	ModelID  string
	Content  string
	Usage    Usage
}

// Ensemble runs the generator providers for a Power-mode phase in parallel.
// Excluded providers are skipped (e.g. code provider must not review its own output).
// Returns all successful results; the caller selects the winner.
func (r *Router) Ensemble(ctx context.Context, phase BuildPhase, messages []Message, system string, exclude ...string) []EnsembleResult {
	pe, ok := powerEnsembleTable[phase]
	if !ok {
		return nil
	}

	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}

	// Collect available generator slots
	type slot struct {
		name    string
		modelID string
		p       Provider
	}
	var slots []slot
	for _, name := range pe.Generators {
		if excluded[name] {
			continue
		}
		if blocked, remaining := r.isCircuitOpen(name); blocked {
			r.emitTrace(TraceEvent{
				Event:    "ensemble_generator_skipped",
				Phase:    buildPhaseName(phase),
				Selected: name,
				Detail:   circuitOpenDetail(name, remaining),
			})
			continue
		}
		p, modelID, err := r.tryBuildProvider(name, TierPremium)
		if err != nil || !p.IsAvailable() {
			continue
		}
		slots = append(slots, slot{name, modelID, p})
	}
	if len(slots) == 0 {
		r.emitTrace(TraceEvent{
			Event:  "ensemble_no_generators",
			Phase:  buildPhaseName(phase),
			Detail: "no available generator provider",
		})
		return nil
	}
	r.emitTrace(TraceEvent{
		Event:  "ensemble_start",
		Phase:  buildPhaseName(phase),
		Detail: "judge=" + pe.Judge,
	})

	maxTokens := maxTokensForPhase(phase)
	results := make([]EnsembleResult, len(slots))
	var wg sync.WaitGroup
	for i, s := range slots {
		wg.Add(1)
		go func(idx int, sl slot) {
			defer wg.Done()
			if allow, remaining := r.beforeProviderAttempt(sl.name); !allow {
				r.emitTrace(TraceEvent{
					Event:    "ensemble_generator_skipped",
					Phase:    buildPhaseName(phase),
					Selected: sl.name,
					ModelID:  sl.modelID,
					Detail:   circuitOpenDetail(sl.name, remaining),
				})
				return
			}
			start := time.Now()
			attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), phase, sl.name)
			content, usage, err := sl.p.Chat(attemptCtx, messages, system, maxTokens)
			attemptCancel()
			if err != nil {
				r.usage.RecordFailure(sl.name)
				if opened, until := r.recordProviderFailureForErr(sl.name, err); opened {
					r.emitTrace(TraceEvent{
						Event:    "circuit_opened",
						Phase:    buildPhaseName(phase),
						Selected: sl.name,
						ModelID:  sl.modelID,
						Detail:   circuitOpenDetail(sl.name, time.Until(until)),
					})
				}
				r.emitTrace(TraceEvent{
					Event:      "ensemble_generator_error",
					Phase:      buildPhaseName(phase),
					Selected:   sl.name,
					ModelID:    sl.modelID,
					DurationMS: time.Since(start).Milliseconds(),
					Error:      err.Error(),
				})
				return
			}
			r.usage.Increment(sl.name)
			r.recordProviderSuccess(sl.name)
			r.emitTrace(TraceEvent{
				Event:      "ensemble_generator_success",
				Phase:      buildPhaseName(phase),
				Selected:   sl.name,
				ModelID:    sl.modelID,
				DurationMS: time.Since(start).Milliseconds(),
			})
			results[idx] = EnsembleResult{sl.name, sl.modelID, content, usage}
		}(i, s)
	}
	wg.Wait()

	var out []EnsembleResult
	for _, res := range results {
		if res.Content != "" {
			out = append(out, res)
		}
	}
	r.emitTrace(TraceEvent{
		Event:  "ensemble_complete",
		Phase:  buildPhaseName(phase),
		Detail: fmt.Sprintf("success=%d/%d", len(out), len(slots)),
	})
	return out
}

// judgeSelect asks the designated judge provider to pick the best result from an ensemble.
// The judge is asked to declare the winner on the first line as "WINNER: N", making
// attribution reliable and independent of the content. Records quality outcomes:
// the winning generator gets a success signal. Falls back to first result on error.
func (r *Router) judgeSelect(ctx context.Context, phase BuildPhase, results []EnsembleResult) EnsembleResult {
	if len(results) == 1 {
		// Single result — treat it as a success (no competition needed).
		r.usage.RecordQualityOutcome(phase, results[0].Provider, true)
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_single_result",
			Phase:    buildPhaseName(phase),
			Selected: results[0].Provider,
		})
		return results[0]
	}

	pe, ok := powerEnsembleTable[phase]
	if !ok {
		r.emitTrace(TraceEvent{
			Event:  "judge_skipped_missing_config",
			Phase:  buildPhaseName(phase),
			Detail: "power ensemble config missing",
		})
		return results[0]
	}

	judgeP, judgeModelID, err := r.tryBuildProvider(pe.Judge, TierPremium)
	if err != nil || !judgeP.IsAvailable() {
		reason := "judge unavailable"
		if err != nil {
			reason = err.Error()
		}
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_unavailable",
			Phase:    buildPhaseName(phase),
			Selected: pe.Judge,
			Error:    reason,
		})
		return results[0]
	}
	if blocked, remaining := r.isCircuitOpen(pe.Judge); blocked {
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_unavailable",
			Phase:    buildPhaseName(phase),
			Selected: pe.Judge,
			Detail:   circuitOpenDetail(pe.Judge, remaining),
		})
		return results[0]
	}
	if allow, remaining := r.beforeProviderAttempt(pe.Judge); !allow {
		r.emitTrace(TraceEvent{
			Event:    "judge_skipped_unavailable",
			Phase:    buildPhaseName(phase),
			Selected: pe.Judge,
			Detail:   circuitOpenDetail(pe.Judge, remaining),
		})
		return results[0]
	}

	var prompt strings.Builder
	for i, res := range results {
		prompt.WriteString(fmt.Sprintf("=== Option %d ===\n%s\n\n", i+1, res.Content))
	}
	// Ask judge to declare which option won on the very first line so attribution
	// is unambiguous regardless of the content (which might mention provider names).
	prompt.WriteString(fmt.Sprintf(
		"First line must be exactly \"WINNER: <N>\" where N is 1–%d.\nThen output the complete chosen option, completely unchanged:",
		len(results),
	))

	judgeMessages := []Message{{Role: "user", Content: prompt.String()}}
	judgeStart := time.Now()
	attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ctx, r.effectiveMode(), phase, pe.Judge)
	content, usage, err := judgeP.Chat(attemptCtx, judgeMessages, judgeSystemFor(phase), maxTokensForPhase(phase))
	attemptCancel()
	if err != nil {
		r.usage.RecordFailure(pe.Judge)
		if opened, until := r.recordProviderFailureForErr(pe.Judge, err); opened {
			r.emitTrace(TraceEvent{
				Event:    "circuit_opened",
				Phase:    buildPhaseName(phase),
				Selected: pe.Judge,
				ModelID:  judgeModelID,
				Detail:   circuitOpenDetail(pe.Judge, time.Until(until)),
			})
		}
		r.emitTrace(TraceEvent{
			Event:      "judge_error",
			Phase:      buildPhaseName(phase),
			Selected:   pe.Judge,
			ModelID:    judgeModelID,
			DurationMS: time.Since(judgeStart).Milliseconds(),
			Error:      err.Error(),
		})
		return results[0]
	}
	r.recordProviderSuccess(pe.Judge)
	r.emitTrace(TraceEvent{
		Event:      "judge_success",
		Phase:      buildPhaseName(phase),
		Selected:   pe.Judge,
		ModelID:    judgeModelID,
		DurationMS: time.Since(judgeStart).Milliseconds(),
	})

	r.usage.Increment(pe.Judge)

	// Parse "WINNER: N" from first line for reliable winner attribution.
	// If parsing fails, default to the first result.
	winner := results[0]
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		firstLine := strings.TrimSpace(content[:idx])
		if strings.HasPrefix(firstLine, "WINNER:") {
			numStr := strings.TrimSpace(strings.TrimPrefix(firstLine, "WINNER:"))
			if n, parseErr := strconv.Atoi(numStr); parseErr == nil && n >= 1 && n <= len(results) {
				winner = results[n-1]
			}
		}
	}
	r.emitTrace(TraceEvent{
		Event:    "judge_winner_selected",
		Phase:    buildPhaseName(phase),
		Selected: winner.Provider,
		ModelID:  winner.ModelID,
	})
	r.usage.RecordQualityOutcome(phase, winner.Provider, true)

	// Return the winning generator's original content and model for correctness.
	// Judge usage is returned separately so ChatBest can accumulate it.
	return EnsembleResult{
		Provider: winner.Provider, // attribution: generator that was selected
		ModelID:  winner.ModelID,  // winning generator's model (for display)
		Content:  winner.Content,  // original generator output, not judge's reproduction
		Usage:    usage,           // judge's usage (for cost tracking)
	}
}

// RecordQualityOutcome exposes the quality feedback mechanism to the app layer.
// Call with success=true when a provider's output passes validation (tests pass, LGTM review).
// Call with success=false when the output fails validation (auto-fix triggered, test failure).
func (r *Router) RecordQualityOutcome(phase BuildPhase, provider string, success bool) {
	r.usage.RecordQualityOutcome(phase, provider, success)
}

// judgeSystemFor returns a phase-specific system prompt for the judge.
func judgeSystemFor(phase BuildPhase) string {
	switch phase {
	case PhaseCode:
		return "You are an expert code evaluator. Given multiple implementations of the same task, select the most complete, correct, and well-structured one. Output ONLY the chosen implementation, completely unchanged, preserving all --- FILE: --- markers."
	case PhaseReview:
		return "You are an expert code reviewer. Given multiple code reviews, select or synthesize the most thorough and actionable one. Output ONLY the best review, completely unchanged."
	case PhasePlan:
		return "You are an expert software architect. Given multiple project plans, select the most detailed and feasible one. Output ONLY the chosen plan, completely unchanged."
	case PhaseFix:
		return "You are an expert debugger. Given multiple fixes for the same error, select the most correct and minimal fix. Output ONLY the chosen fix, completely unchanged, preserving all --- FILE: --- markers."
	default:
		return "You are an expert evaluator. Given multiple responses to the same request, select the best one. Output ONLY the chosen response, completely unchanged."
	}
}

// ChatBest selects the best provider response for a build phase.
//   - Power mode: runs all ensemble generators in parallel, then uses a cross-model
//     judge to select the winner.
//   - Other modes: uses Thompson Sampling to adaptively select the primary provider
//     from the buildStrategyTable candidates, then delegates to ChatWith.
func (r *Router) ChatBest(ctx context.Context, phase BuildPhase, messages []Message, system string, exclude ...string) (string, Usage, RouteResult, error) {
	if r.effectiveMode() != ModePower {
		return r.ChatWith(ctx, r.BuildProviderForAdaptive(phase), phase, messages, system, exclude...)
	}

	r.emitTrace(TraceEvent{
		Event:  "chat_best_power_start",
		Phase:  buildPhaseName(phase),
		Detail: "mode=power",
	})

	results := r.Ensemble(ctx, phase, messages, system, exclude...)
	if len(results) == 0 {
		// Ensemble had no available generators — fall back to adaptive routing
		r.emitTrace(TraceEvent{
			Event:  "chat_best_power_fallback_adaptive",
			Phase:  buildPhaseName(phase),
			Detail: "ensemble returned zero results",
		})
		return r.ChatWith(ctx, r.BuildProviderForAdaptive(phase), phase, messages, system, exclude...)
	}

	best := r.judgeSelect(ctx, phase, results)

	// Accumulate usage across all ensemble calls: generators + judge.
	var total Usage
	for _, res := range results {
		total.InputTokens += res.Usage.InputTokens
		total.OutputTokens += res.Usage.OutputTokens
		total.Cost += res.Usage.Cost
	}
	// Add judge's usage (previously missing).
	total.InputTokens += best.Usage.InputTokens
	total.OutputTokens += best.Usage.OutputTokens
	total.Cost += best.Usage.Cost
	total.Provider = best.Provider // winning generator
	total.Model = best.ModelID

	r.emitTrace(TraceEvent{
		Event:    "chat_best_power_selected",
		Phase:    buildPhaseName(phase),
		Selected: best.Provider,
		ModelID:  best.ModelID,
		Detail:   fmt.Sprintf("candidates=%d", len(results)),
	})

	return best.Content, total, RouteResult{
		Actual:    best.Provider,
		ModelID:   best.ModelID,
		Requested: "ensemble",
	}, nil
}
