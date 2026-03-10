// execute.go — Unified execution pipeline for Chat, ChatWith, and ChatStream.
// This file extracts the common retry/fallback/circuit-breaker pattern shared
// across the three methods into reusable helpers.
package model

import (
	"context"
	"fmt"
	"time"
)

// attemptLabels holds the trace event name templates for a particular call site.
// Each method (Chat, ChatWith, ChatStream) passes its own labels so that trace
// event names are preserved exactly.
type attemptLabels struct {
	attemptSuccess  string // e.g. "chat_attempt_success"
	attemptError    string // e.g. "chat_attempt_error"
	fallbackSkipped string // e.g. "chat_fallback_skipped"
	failedAll       string // e.g. "chat_failed_all"
}

// attemptIdentity identifies a single provider candidate in the pipeline.
type attemptIdentity struct {
	name       string
	modelID    string
	provider   Provider
	isFallback bool
}

// attemptContext bundles the per-call parameters needed by tryProvider.
type attemptContext struct {
	ctx       context.Context
	messages  []Message
	system    string
	maxTokens int
	phase     BuildPhase
	mode      UsageMode
	requested string // name of the originally-requested provider (for traces)
	task      TaskType
	labels    attemptLabels
	// taskLabel and phaseLabel are for trace events — exactly one is set.
	taskLabel  string
	phaseLabel string
}

// tryProviderResult is the outcome of a single provider attempt.
type tryProviderResult struct {
	content string
	usage   Usage
	route   RouteResult
	err     error
	skipped bool // true if the provider was skipped (circuit open, unavailable, etc.)
}

// tryProvider attempts a single Chat call against the given provider, handling
// circuit breaker checks, timeout context, success/failure recording, and trace emission.
func (r *Router) tryProvider(ac *attemptContext, id attemptIdentity) tryProviderResult {
	// Circuit breaker pre-check
	if allow, remaining := r.beforeProviderAttempt(id.name); !allow {
		detail := circuitOpenDetail(id.name, remaining)
		r.emitTrace(TraceEvent{
			Event:      ac.labels.fallbackSkipped,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    id.modelID,
			IsFallback: id.isFallback,
			Detail:     detail,
		})
		return tryProviderResult{skipped: true, err: fmt.Errorf("%s", detail)}
	}

	start := time.Now()
	attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ac.ctx, ac.mode, ac.phase, id.name)
	content, usage, chatErr := id.provider.Chat(attemptCtx, ac.messages, ac.system, ac.maxTokens)
	attemptCancel()

	if chatErr == nil {
		r.usage.Increment(id.name)
		r.recordProviderSuccess(id.name)
		r.emitTrace(TraceEvent{
			Event:      ac.labels.attemptSuccess,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    id.modelID,
			IsFallback: id.isFallback,
			DurationMS: time.Since(start).Milliseconds(),
		})
		return tryProviderResult{
			content: content,
			usage:   usage,
			route: RouteResult{
				Provider:   id.provider,
				ModelID:    id.modelID,
				Requested:  ac.requested,
				Actual:     id.name,
				IsFallback: id.isFallback,
			},
		}
	}

	r.emitTrace(TraceEvent{
		Event:      ac.labels.attemptError,
		Task:       ac.taskLabel,
		Phase:      ac.phaseLabel,
		Requested:  ac.requested,
		Selected:   id.name,
		ModelID:    id.modelID,
		IsFallback: id.isFallback,
		DurationMS: time.Since(start).Milliseconds(),
		Error:      chatErr.Error(),
	})
	r.usage.RecordFailure(id.name)
	if opened, until := r.recordProviderFailureForErr(id.name, chatErr); opened {
		r.emitTrace(TraceEvent{
			Event:    "circuit_opened",
			Task:     ac.taskLabel,
			Phase:    ac.phaseLabel,
			Selected: id.name,
			ModelID:  id.modelID,
			Detail:   circuitOpenDetail(id.name, time.Until(until)),
		})
	}
	return tryProviderResult{err: chatErr}
}

// candidateResolver is a function that returns (provider, modelID, error) for a
// given candidate name. Chat/ChatStream use resolveProvider; ChatWith uses tryBuildProvider.
type candidateResolver func(name, modelID string) (Provider, string, error)

// resolverForMode returns a candidateResolver that uses r.resolveProvider.
// The returned modelID is always the input modelID (resolveProvider doesn't change it).
func (r *Router) resolverForMode() candidateResolver {
	return func(name, modelID string) (Provider, string, error) {
		p, err := r.resolveProvider(name, modelID)
		return p, modelID, err
	}
}

// fallbackCandidate represents a provider to try during the fallback loop.
type fallbackCandidate struct {
	name    string
	modelID string
}

// iterateFallbackCandidates iterates over fallback candidates, resolves each using
// the given resolver, checks circuit breaker and availability, and calls tryProvider.
// Returns the first successful result or the accumulated first error.
func (r *Router) iterateFallbackCandidates(ac *attemptContext, candidates []fallbackCandidate, resolve candidateResolver) (string, Usage, RouteResult, error) {
	var firstErr error

	for _, c := range candidates {
		if blocked, remaining := r.isCircuitOpen(c.name); blocked {
			r.emitTrace(TraceEvent{
				Event:      ac.labels.fallbackSkipped,
				Task:       ac.taskLabel,
				Phase:      ac.phaseLabel,
				Requested:  ac.requested,
				Selected:   c.name,
				ModelID:    c.modelID,
				IsFallback: true,
				Detail:     circuitOpenDetail(c.name, remaining),
			})
			continue
		}

		p, modelID, resolveErr := resolve(c.name, c.modelID)
		if resolveErr != nil {
			r.emitTrace(TraceEvent{
				Event:     ac.labels.fallbackSkipped,
				Task:      ac.taskLabel,
				Phase:     ac.phaseLabel,
				Requested: ac.requested,
				Selected:  c.name,
				ModelID:   c.modelID,
				Error:     resolveErr.Error(),
			})
			continue
		}
		if !p.IsAvailable() {
			r.emitTrace(TraceEvent{
				Event:     ac.labels.fallbackSkipped,
				Task:      ac.taskLabel,
				Phase:     ac.phaseLabel,
				Requested: ac.requested,
				Selected:  c.name,
				ModelID:   c.modelID,
				Detail:    "provider unavailable",
			})
			continue
		}

		res := r.tryProvider(ac, attemptIdentity{
			name:       c.name,
			modelID:    modelID,
			provider:   p,
			isFallback: true,
		})
		if res.skipped {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		if res.err == nil {
			return res.content, res.usage, res.route, nil
		}
		if firstErr == nil {
			firstErr = res.err
		}
	}

	return "", Usage{}, RouteResult{}, firstErr
}

// routeAndExecute implements the unified Chat/ChatWith execution pipeline.
// It tries the primary provider, then iterates fallback candidates on failure.
func (r *Router) routeAndExecute(ac *attemptContext, result RouteResult, fallbacks []fallbackCandidate, resolve candidateResolver) (string, Usage, RouteResult, error) {
	// Try primary
	primaryRes := r.tryProvider(ac, attemptIdentity{
		name:       result.Actual,
		modelID:    result.ModelID,
		provider:   result.Provider,
		isFallback: result.IsFallback,
	})
	if !primaryRes.skipped && primaryRes.err == nil {
		return primaryRes.content, primaryRes.usage, primaryRes.route, nil
	}
	firstErr := primaryRes.err

	// Try fallbacks
	content, usage, route, fbErr := r.iterateFallbackCandidates(ac, fallbacks, resolve)
	if fbErr == nil {
		return content, usage, route, nil
	}
	if firstErr == nil {
		firstErr = fbErr
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no provider attempt was possible")
	}

	r.emitTrace(TraceEvent{
		Event:     ac.labels.failedAll,
		Task:      ac.taskLabel,
		Phase:     ac.phaseLabel,
		Requested: ac.requested,
		Selected:  result.Actual,
		Error:     firstErr.Error(),
	})
	return "", Usage{}, result, firstErr
}

// tryStreamProvider attempts a single ChatStream call against the given provider.
// On success the returned channel is wrapped with withStreamAttemptCancel.
type tryStreamResult struct {
	ch      <-chan StreamChunk
	route   RouteResult
	err     error
	skipped bool
}

func (r *Router) tryStreamProvider(ac *attemptContext, id attemptIdentity) tryStreamResult {
	// Circuit breaker pre-check
	if allow, remaining := r.beforeProviderAttempt(id.name); !allow {
		detail := circuitOpenDetail(id.name, remaining)
		r.emitTrace(TraceEvent{
			Event:      ac.labels.fallbackSkipped,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    id.modelID,
			IsFallback: id.isFallback,
			Detail:     detail,
		})
		return tryStreamResult{skipped: true, err: fmt.Errorf("%s", detail)}
	}

	start := time.Now()
	attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ac.ctx, ac.mode, ac.phase, id.name)
	ch, streamErr := id.provider.ChatStream(attemptCtx, ac.messages, ac.system, ac.maxTokens)

	if streamErr == nil {
		r.usage.Increment(id.name)
		r.recordProviderSuccess(id.name)
		r.emitTrace(TraceEvent{
			Event:      ac.labels.attemptSuccess,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    id.modelID,
			IsFallback: id.isFallback,
			DurationMS: time.Since(start).Milliseconds(),
		})
		return tryStreamResult{
			ch: withStreamAttemptCancel(ch, attemptCancel),
			route: RouteResult{
				Provider:   id.provider,
				ModelID:    id.modelID,
				Requested:  ac.requested,
				Actual:     id.name,
				IsFallback: id.isFallback,
			},
		}
	}

	attemptCancel()
	r.emitTrace(TraceEvent{
		Event:      ac.labels.attemptError,
		Task:       ac.taskLabel,
		Phase:      ac.phaseLabel,
		Requested:  ac.requested,
		Selected:   id.name,
		ModelID:    id.modelID,
		IsFallback: id.isFallback,
		DurationMS: time.Since(start).Milliseconds(),
		Error:      streamErr.Error(),
	})
	r.usage.RecordFailure(id.name)
	if opened, until := r.recordProviderFailureForErr(id.name, streamErr); opened {
		r.emitTrace(TraceEvent{
			Event:    "circuit_opened",
			Task:     ac.taskLabel,
			Phase:    ac.phaseLabel,
			Selected: id.name,
			ModelID:  id.modelID,
			Detail:   circuitOpenDetail(id.name, time.Until(until)),
		})
	}
	return tryStreamResult{err: streamErr}
}

// routeAndExecuteStream implements the unified ChatStream execution pipeline.
func (r *Router) routeAndExecuteStream(ac *attemptContext, result RouteResult, fallbacks []fallbackCandidate, resolve candidateResolver) (<-chan StreamChunk, RouteResult, error) {
	// Try primary
	primaryRes := r.tryStreamProvider(ac, attemptIdentity{
		name:       result.Actual,
		modelID:    result.ModelID,
		provider:   result.Provider,
		isFallback: result.IsFallback,
	})
	if !primaryRes.skipped && primaryRes.err == nil {
		return primaryRes.ch, primaryRes.route, nil
	}
	firstErr := primaryRes.err

	// Try fallbacks
	for _, c := range fallbacks {
		if blocked, remaining := r.isCircuitOpen(c.name); blocked {
			r.emitTrace(TraceEvent{
				Event:      ac.labels.fallbackSkipped,
				Task:       ac.taskLabel,
				Phase:      ac.phaseLabel,
				Requested:  ac.requested,
				Selected:   c.name,
				ModelID:    c.modelID,
				IsFallback: true,
				Detail:     circuitOpenDetail(c.name, remaining),
			})
			continue
		}

		p, modelID, resolveErr := resolve(c.name, c.modelID)
		if resolveErr != nil {
			r.emitTrace(TraceEvent{
				Event:     ac.labels.fallbackSkipped,
				Task:      ac.taskLabel,
				Phase:     ac.phaseLabel,
				Requested: ac.requested,
				Selected:  c.name,
				ModelID:   c.modelID,
				Error:     resolveErr.Error(),
			})
			continue
		}
		if !p.IsAvailable() {
			r.emitTrace(TraceEvent{
				Event:     ac.labels.fallbackSkipped,
				Task:      ac.taskLabel,
				Phase:     ac.phaseLabel,
				Requested: ac.requested,
				Selected:  c.name,
				ModelID:   c.modelID,
				Detail:    "provider unavailable",
			})
			continue
		}

		res := r.tryStreamProvider(ac, attemptIdentity{
			name:       c.name,
			modelID:    modelID,
			provider:   p,
			isFallback: true,
		})
		if res.skipped {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		if res.err == nil {
			return res.ch, res.route, nil
		}
		if firstErr == nil {
			firstErr = res.err
		}
	}

	if firstErr == nil {
		firstErr = fmt.Errorf("no provider attempt was possible")
	}

	r.emitTrace(TraceEvent{
		Event:     ac.labels.failedAll,
		Task:      ac.taskLabel,
		Phase:     ac.phaseLabel,
		Requested: ac.requested,
		Selected:  result.Actual,
		Error:     firstErr.Error(),
	})
	return nil, result, firstErr
}

// chatFallbackCandidates builds the fallback candidate list for Chat/ChatStream.
// It handles both mode-based and legacy fallback ordering.
func (r *Router) chatFallbackCandidates(task TaskType, primaryName string) []fallbackCandidate {
	if r.ModeSet() {
		entry, err := r.modeEntry(task)
		if err != nil {
			return nil
		}
		excluded := map[string]bool{primaryName: true}
		modeCands := r.modeCandidates(entry, excluded, taskToBuildPhase(task))
		out := make([]fallbackCandidate, 0, len(modeCands))
		for _, c := range modeCands {
			out = append(out, fallbackCandidate{name: c.name, modelID: c.modelID})
		}
		return out
	}

	// Legacy fallback
	out := make([]fallbackCandidate, 0, len(fallbackOrder))
	for _, name := range fallbackOrder {
		if name == primaryName {
			continue
		}
		// For legacy path, check provider existence and availability inline
		// (the resolver will also check, but we skip early for non-existent providers)
		if p, ok := r.providers[name]; ok && p.IsAvailable() {
			out = append(out, fallbackCandidate{name: name})
		}
	}
	return out
}

// legacyResolver returns a candidateResolver for the legacy (non-mode) path.
// It looks up providers from the registered providers map directly.
func (r *Router) legacyResolver() candidateResolver {
	return func(name, _ string) (Provider, string, error) {
		if p, ok := r.providers[name]; ok {
			return p, "", nil
		}
		return nil, "", fmt.Errorf("provider %s not found", name)
	}
}

// chatWithFallbackCandidates builds fallback candidates for ChatWith (build phase).
func (r *Router) chatWithFallbackCandidates(phase BuildPhase, primaryName string, exclude []string) ([]fallbackCandidate, ModelTier) {
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}
	excluded[primaryName] = true

	bs, candidates, err := r.buildPhaseCandidates(phase, excluded)
	if err != nil {
		return nil, TierCheap
	}

	out := make([]fallbackCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.name == primaryName || excluded[c.name] {
			continue
		}
		out = append(out, fallbackCandidate{name: c.name, modelID: c.modelID})
	}
	return out, bs.Tier
}

// buildProviderResolver returns a candidateResolver that uses tryBuildProvider with the given tier.
func (r *Router) buildProviderResolver(tier ModelTier) candidateResolver {
	return func(name, _ string) (Provider, string, error) {
		return r.tryBuildProvider(name, tier)
	}
}
