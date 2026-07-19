// execute.go — Unified execution pipeline for Chat, ChatWith, and ChatStream.
// This file extracts the common retry/fallback/circuit-breaker pattern shared
// across the three methods into reusable helpers.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errNoFallbackAttempted marks that the fallback iterator ended without a single
// successful attempt — an empty candidate list, or every candidate skipped. It
// lets routeAndExecute distinguish "a fallback produced output" (nil error →
// success) from "no fallback succeeded", so the primary's fail-closed error
// (e.g. ErrNoUntrustedSafeProvider) is preserved instead of being masked by an
// empty-content success return.
var errNoFallbackAttempted = errors.New("no fallback provider attempt succeeded")

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
	actualModelID := reportedModelID(id.provider, id.modelID)
	// Fail-closed defense in depth: never execute a provider that is not
	// untrusted-repo-safe when untrusted mode is active, regardless of how it
	// reached the execution pipeline.
	if !r.untrustedRepoSafe(id.provider) {
		r.emitTrace(TraceEvent{
			Event:      ac.labels.fallbackSkipped,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    actualModelID,
			IsFallback: id.isFallback,
			Detail:     untrustedRepoSkipDetail(id.name),
		})
		return tryProviderResult{skipped: true, err: ErrNoUntrustedSafeProvider}
	}
	if callerErr := ac.ctx.Err(); callerErr != nil {
		r.emitTrace(TraceEvent{
			Event:      ac.labels.fallbackSkipped,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    actualModelID,
			IsFallback: id.isFallback,
			Error:      callerErr.Error(),
			Detail:     "caller context already ended",
		})
		return tryProviderResult{skipped: true, err: callerErr}
	}
	// Circuit breaker pre-check
	if allow, remaining := r.beforeProviderAttempt(id.name); !allow {
		detail := circuitOpenDetail(id.name, remaining)
		r.emitTrace(TraceEvent{
			Event:      ac.labels.fallbackSkipped,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    actualModelID,
			IsFallback: id.isFallback,
			Detail:     detail,
		})
		return tryProviderResult{skipped: true, err: fmt.Errorf("%s", detail)}
	}

	start := time.Now()
	attemptCtx, attemptCancel := withProviderAttemptTimeoutFor(ac.ctx, ac.mode, ac.phase, id.name)
	attemptCtx = ContextWithUsageMode(attemptCtx, ac.mode)
	// Inject this Router's price snapshot so a provider shared by several Routers
	// prices from the calling Router's overrides, not whichever Router registered last.
	attemptCtx = contextWithCostTable(attemptCtx, r.routingTables())
	// Inject model ID into context so CLI providers can use --model for per-call selection.
	if id.modelID != "" {
		attemptCtx = ContextWithModel(attemptCtx, id.modelID)
	}
	content, usage, chatErr := id.provider.Chat(attemptCtx, ac.messages, ac.system, ac.maxTokens)
	attemptCancel()

	if chatErr == nil {
		// Remote and API transports may return a more authoritative model ID in
		// their usage payload. Use it when routing had no concrete model identity.
		if actualModelID == "" && usage.Model != "" {
			actualModelID = usage.Model
		}
		r.usage.Increment(id.name)
		r.recordProviderSuccess(id.name)
		r.emitTrace(TraceEvent{
			Event:      ac.labels.attemptSuccess,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    actualModelID,
			IsFallback: id.isFallback,
			DurationMS: time.Since(start).Milliseconds(),
		})
		return tryProviderResult{
			content: content,
			usage:   usage,
			route: RouteResult{
				Provider:   id.provider,
				ModelID:    actualModelID,
				Requested:  ac.requested,
				Actual:     id.name,
				IsFallback: id.isFallback,
			},
		}
	}

	detail := ""
	providerFailure := true
	if ac.ctx.Err() != nil {
		// The caller's context ending is not provider-health evidence, even if a
		// transport wraps the cancellation in a generic CLI/config error.
		providerFailure = false
		detail = "request canceled by caller"
	}
	r.emitTrace(TraceEvent{
		Event:      ac.labels.attemptError,
		Task:       ac.taskLabel,
		Phase:      ac.phaseLabel,
		Requested:  ac.requested,
		Selected:   id.name,
		ModelID:    actualModelID,
		IsFallback: id.isFallback,
		DurationMS: time.Since(start).Milliseconds(),
		Error:      chatErr.Error(),
		Detail:     detail,
	})
	if !providerFailure {
		return tryProviderResult{err: chatErr}
	}
	r.usage.RecordFailure(id.name)
	if opened, until := r.recordProviderFailureForErr(id.name, chatErr); opened {
		r.emitTrace(TraceEvent{
			Event:    "circuit_opened",
			Task:     ac.taskLabel,
			Phase:    ac.phaseLabel,
			Selected: id.name,
			ModelID:  actualModelID,
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
		if callerErr := ac.ctx.Err(); callerErr != nil {
			return "", Usage{}, RouteResult{}, callerErr
		}
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
		if !r.untrustedRepoSafe(p) {
			r.emitTrace(TraceEvent{
				Event:      ac.labels.fallbackSkipped,
				Task:       ac.taskLabel,
				Phase:      ac.phaseLabel,
				Requested:  ac.requested,
				Selected:   c.name,
				ModelID:    modelID,
				IsFallback: true,
				Detail:     untrustedRepoSkipDetail(c.name),
			})
			if firstErr == nil {
				firstErr = ErrNoUntrustedSafeProvider
			}
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

	// The loop produced no successful attempt. Never return a nil error here: a
	// nil return means "a fallback succeeded" to routeAndExecute, which would mask
	// the primary's fail-closed sentinel with an empty-content success. When the
	// list was empty or every candidate was skipped without an error, surface the
	// errNoFallbackAttempted sentinel so the caller keeps the primary's error.
	if firstErr == nil {
		firstErr = errNoFallbackAttempted
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
	if callerErr := ac.ctx.Err(); callerErr != nil {
		return "", Usage{}, result, callerErr
	}

	// Try fallbacks
	content, usage, route, fbErr := r.iterateFallbackCandidates(ac, fallbacks, resolve)
	if fbErr == nil {
		return content, usage, route, nil
	}
	if callerErr := ac.ctx.Err(); callerErr != nil {
		return "", Usage{}, result, callerErr
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
// It waits for the first meaningful chunk so an error before any output can still
// use the normal fallback chain. Once output starts, the returned channel records
// success or failure only when the stream reaches a terminal state.
type tryStreamResult struct {
	ch      <-chan StreamChunk
	route   RouteResult
	err     error
	skipped bool
}

const streamForwardTimeout = 30 * time.Second

func (r *Router) recordStreamAttemptSuccess(ac *attemptContext, id attemptIdentity, start time.Time) {
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
		Detail:     "stream completed",
	})
}

func (r *Router) recordStreamAttemptError(ac *attemptContext, id attemptIdentity, start time.Time, streamErr error) {
	detail := "stream terminated with error"
	providerFailure := true
	if ac.ctx.Err() != nil {
		// A caller cancellation/deadline is not evidence that the provider is
		// unhealthy. Keep it visible in traces without poisoning routing feedback.
		providerFailure = false
		detail = "stream canceled by caller"
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
		Error:      streamErr.Error(),
		Detail:     detail,
	})
	if !providerFailure {
		return
	}
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
}

// observeStreamAttempt forwards a stream after its first meaningful chunk has
// committed the route. Provider health is updated before the terminal chunk is
// exposed to the consumer, so callers observing completion also observe the
// corresponding breaker and usage state.
func (r *Router) observeStreamAttempt(
	ac *attemptContext,
	id attemptIdentity,
	start time.Time,
	attemptCtx context.Context,
	attemptCancel context.CancelFunc,
	source <-chan StreamChunk,
	first StreamChunk,
) <-chan StreamChunk {
	out := make(chan StreamChunk, 1)

	go func() {
		defer close(out)
		defer attemptCancel()

		forward := func(chunk StreamChunk) bool {
			timer := time.NewTimer(streamForwardTimeout)
			defer timer.Stop()
			select {
			case out <- chunk:
				return true
			case <-ac.ctx.Done():
				return false
			case <-timer.C:
				// The consumer stopped reading. Cancel the provider attempt, but do
				// not attribute a client-side abandonment as provider success/failure.
				return false
			}
		}

		chunk := first
		for {
			if chunk.Error != nil {
				r.recordStreamAttemptError(ac, id, start, chunk.Error)
				_ = forward(chunk)
				return
			}
			if chunk.Done {
				r.recordStreamAttemptSuccess(ac, id, start)
				_ = forward(chunk)
				return
			}
			if !forward(chunk) {
				return
			}

			select {
			case next, ok := <-source:
				if !ok {
					streamErr := fmt.Errorf("%s stream closed before completion", id.name)
					if err := attemptCtx.Err(); err != nil {
						streamErr = err
					}
					r.recordStreamAttemptError(ac, id, start, streamErr)
					_ = forward(StreamChunk{Error: streamErr, Done: true})
					return
				}
				chunk = next
			case <-attemptCtx.Done():
				streamErr := attemptCtx.Err()
				r.recordStreamAttemptError(ac, id, start, streamErr)
				_ = forward(StreamChunk{Error: streamErr, Done: true})
				return
			}
		}
	}()

	return out
}

func (r *Router) tryStreamProvider(ac *attemptContext, id attemptIdentity) tryStreamResult {
	requestedModelID := id.modelID
	id.modelID = reportedModelID(id.provider, requestedModelID)
	// Fail-closed defense in depth: never stream from a provider that is not
	// untrusted-repo-safe when untrusted mode is active.
	if !r.untrustedRepoSafe(id.provider) {
		r.emitTrace(TraceEvent{
			Event:      ac.labels.fallbackSkipped,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    id.modelID,
			IsFallback: id.isFallback,
			Detail:     untrustedRepoSkipDetail(id.name),
		})
		return tryStreamResult{skipped: true, err: ErrNoUntrustedSafeProvider}
	}
	if callerErr := ac.ctx.Err(); callerErr != nil {
		r.emitTrace(TraceEvent{
			Event:      ac.labels.fallbackSkipped,
			Task:       ac.taskLabel,
			Phase:      ac.phaseLabel,
			Requested:  ac.requested,
			Selected:   id.name,
			ModelID:    id.modelID,
			IsFallback: id.isFallback,
			Error:      callerErr.Error(),
			Detail:     "caller context already ended",
		})
		return tryStreamResult{skipped: true, err: callerErr}
	}

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
	attemptCtx = ContextWithUsageMode(attemptCtx, ac.mode)
	// Inject this Router's price snapshot so a provider shared by several Routers
	// prices from the calling Router's overrides, not whichever Router registered last.
	attemptCtx = contextWithCostTable(attemptCtx, r.routingTables())
	// Inject model ID into context so CLI providers can use --model for per-call selection.
	if requestedModelID != "" {
		attemptCtx = ContextWithModel(attemptCtx, requestedModelID)
	}
	ch, streamErr := id.provider.ChatStream(attemptCtx, ac.messages, ac.system, ac.maxTokens)

	if streamErr != nil {
		attemptCancel()
		r.recordStreamAttemptError(ac, id, start, streamErr)
		return tryStreamResult{err: streamErr}
	}
	if ch == nil {
		attemptCancel()
		streamErr = fmt.Errorf("%s returned a nil stream", id.name)
		r.recordStreamAttemptError(ac, id, start, streamErr)
		return tryStreamResult{err: streamErr}
	}

	route := RouteResult{
		Provider:   id.provider,
		ModelID:    id.modelID,
		Requested:  ac.requested,
		Actual:     id.name,
		IsFallback: id.isFallback,
	}

	// Do not commit the route merely because ChatStream returned a channel. A
	// provider can report transport, quota, or process failures asynchronously.
	// Waiting for the first content/done chunk lets errors before any output use
	// the existing fallback chain without duplicating partial output.
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				streamErr = fmt.Errorf("%s stream closed before completion", id.name)
				if err := attemptCtx.Err(); err != nil {
					streamErr = err
				}
				attemptCancel()
				r.recordStreamAttemptError(ac, id, start, streamErr)
				return tryStreamResult{err: streamErr}
			}
			if chunk.Error != nil {
				attemptCancel()
				r.recordStreamAttemptError(ac, id, start, chunk.Error)
				return tryStreamResult{err: chunk.Error}
			}
			if chunk.Done || chunk.Content != "" {
				return tryStreamResult{
					ch:    r.observeStreamAttempt(ac, id, start, attemptCtx, attemptCancel, ch, chunk),
					route: route,
				}
			}
			// Empty non-terminal chunks carry no observable output and therefore
			// do not commit the route; keep waiting so a following error can fall back.
		case <-attemptCtx.Done():
			attemptCancel()
			streamErr = attemptCtx.Err()
			r.recordStreamAttemptError(ac, id, start, streamErr)
			return tryStreamResult{err: streamErr}
		}
	}
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
	if callerErr := ac.ctx.Err(); callerErr != nil {
		return nil, result, callerErr
	}

	// Try fallbacks
	for _, c := range fallbacks {
		if callerErr := ac.ctx.Err(); callerErr != nil {
			return nil, result, callerErr
		}
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
		if !r.untrustedRepoSafe(p) {
			r.emitTrace(TraceEvent{
				Event:      ac.labels.fallbackSkipped,
				Task:       ac.taskLabel,
				Phase:      ac.phaseLabel,
				Requested:  ac.requested,
				Selected:   c.name,
				ModelID:    modelID,
				IsFallback: true,
				Detail:     untrustedRepoSkipDetail(c.name),
			})
			if firstErr == nil {
				firstErr = ErrNoUntrustedSafeProvider
			}
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
	if callerErr := ac.ctx.Err(); callerErr != nil {
		return nil, result, callerErr
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
	names := r.legacyFallbackCandidates(primaryName)
	out := make([]fallbackCandidate, 0, len(names))
	for _, c := range names {
		// For legacy path, check provider existence and availability inline
		// (the resolver will also check, but we skip early for non-existent providers).
		p, ok := r.getProvider(c.name)
		if !ok {
			continue
		}
		// Untrusted-repo mode: the safety check runs BEFORE IsAvailable so an unsafe
		// CLI is never probed (IsAvailable execs a host health check that inherits the
		// process cwd) merely to build the fallback list — this list is constructed by
		// Chat/ChatStream even when the primary is a safe API provider that succeeds.
		// untrustedRepoSafe is a no-op in trusted mode, so behavior is unchanged there.
		if !r.untrustedRepoSafe(p) {
			continue
		}
		if p.IsAvailable() {
			out = append(out, c)
		}
	}
	return out
}

// legacyResolver returns a candidateResolver for the legacy (non-mode) path.
// It looks up providers from the registered providers map directly.
func (r *Router) legacyResolver() candidateResolver {
	return func(name, modelID string) (Provider, string, error) {
		if p, ok := r.getProvider(name); ok {
			if modelID == "" {
				modelID = r.legacyModelID(name)
			}
			return p, modelID, nil
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
