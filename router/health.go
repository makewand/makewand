// health.go — Circuit breaker wrappers and timeout management.
package router

import (
	"context"
	"errors"
	"strings"
	"time"
)

// healthKey returns the key provider's call outcomes are recorded under: the
// name itself, or the key of a model a client named (Router.healthKeys).
func (r *Router) healthKey(provider string) string {
	if key, ok := r.healthKeys[provider]; ok {
		return key
	}
	return provider
}

// blockedDetail is circuitOpenDetail for provider's refused calls, naming what
// refuses them: the provider's own open circuit or rate-limit block or, for
// the calls of a model a client named, that model's key. A block of one named
// model is then never reported, to the client or in traces, as the provider's.
func (r *Router) blockedDetail(provider string, remaining time.Duration) string {
	if key := r.healthKey(provider); key != provider {
		blocked, _ := r.quotaHardBlocked(provider)
		if !blocked && r.breaker != nil {
			blocked, _ = r.breaker.PeekOpen(provider)
		}
		if !blocked {
			provider = key
		}
	}
	return circuitOpenDetail(provider, remaining)
}

// isCircuitOpen reports whether an open circuit refuses provider's calls: the
// provider's own or, when they are recorded under another key, that key's.
func (r *Router) isCircuitOpen(provider string) (bool, time.Duration) {
	if r.breaker == nil {
		return false, 0
	}
	if open, remaining := r.breaker.PeekOpen(provider); open {
		return true, remaining
	}
	if key := r.healthKey(provider); key != provider {
		return r.breaker.PeekOpen(key)
	}
	return false, 0
}

func (r *Router) beforeProviderAttempt(provider string) (bool, time.Duration) {
	allow, remaining, _ := r.admitProviderAttempt(provider)
	return allow, remaining
}

// admitProviderAttempt is beforeProviderAttempt returning the breaker admission
// ticket that recordProviderSuccess needs to tell the half-open probe apart
// from stale in-flight requests.
func (r *Router) admitProviderAttempt(provider string) (bool, time.Duration, breakerTicket) {
	// Confirmed-exhaustion quota seal is an unbypassable hard gate: a pool that
	// returned a real 429/quota error can't serve until its window resets, so
	// skip it here regardless of which routing path selected it.
	if blocked, until := r.quotaHardBlocked(provider); blocked {
		return false, time.Until(until), breakerTicket{}
	}
	key := r.healthKey(provider)
	if key != provider {
		if blocked, until := r.quotaHardBlocked(key); blocked {
			return false, time.Until(until), breakerTicket{}
		}
	}
	if r.breaker == nil {
		return true, 0, breakerTicket{}
	}
	if key != provider {
		// Calls recorded under their own key still honor the provider's open
		// circuit, but never take its half-open probe: only the provider's own
		// calls can close it.
		if open, remaining := r.breaker.PeekOpen(provider); open {
			return false, remaining, breakerTicket{}
		}
	}
	return r.breaker.Admit(key)
}

func (r *Router) recordProviderSuccess(provider string, ticket breakerTicket) {
	if r.breaker == nil {
		return
	}
	r.breaker.RecordAdmittedSuccess(r.healthKey(provider), ticket)
}

func (r *Router) recordProviderFailureForErr(provider string, callErr error) (bool, time.Time) {
	key := r.healthKey(provider)
	// Feed confirmed quota/rate-limit errors back to the quota layer so the pool
	// is sealed until its window resets, not just circuit-broken for a cooldown.
	r.noteQuotaError(key, callErr)
	if r.breaker == nil {
		return false, time.Time{}
	}
	// Timeouts are usually transient provider saturation/outage signals.
	// Trip the breaker immediately so subsequent requests route elsewhere.
	if isTimeoutErr(callErr) {
		return r.breaker.RecordFailureWeighted(key, defaultCircuitFailureThreshold)
	}
	return r.breaker.RecordFailure(key)
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if ErrorKindOf(err) == ErrorKindTimeout {
		return true
	}
	type timeoutErr interface {
		Timeout() bool
	}
	var te timeoutErr
	if errors.As(err, &te) && te.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "deadline exceeded")
}

// maxTokensForPhase returns the max output tokens for a build phase.
func maxTokensForPhase(phase BuildPhase) int {
	switch phase {
	case PhaseCode:
		return 16384 // full project generation needs more than TaskCode's 8192
	case PhaseFix:
		return 8192
	case PhasePlan, PhaseReview:
		return 4096
	default:
		return 8192
	}
}

func buildPhaseAttemptTimeout(phase BuildPhase) time.Duration {
	switch phase {
	case PhasePlan:
		return 90 * time.Second
	case PhaseCode:
		return 180 * time.Second
	case PhaseReview:
		return 45 * time.Second
	case PhaseFix:
		return 60 * time.Second
	default:
		return 90 * time.Second
	}
}

func providerAttemptTimeout(mode UsageMode, phase BuildPhase, provider string) time.Duration {
	provider = strings.ToLower(strings.TrimSpace(provider))

	// Fast mode: tight per-attempt budgets to fail fast and preserve fallback room.
	if mode == ModeFast {
		switch phase {
		case PhaseCode:
			return 45 * time.Second
		case PhaseReview:
			return 35 * time.Second
		default:
			return 30 * time.Second
		}
	}

	// Balanced mode: moderate budgets — shorter than default for review/plan.
	if mode == ModeBalanced {
		switch phase {
		case PhaseCode:
			return 90 * time.Second
		case PhaseReview:
			return 40 * time.Second
		case PhaseFix:
			return 90 * time.Second
		default:
			return 60 * time.Second
		}
	}

	// Power mode: generous budgets — ensemble calls are parallel so total wall
	// time is bounded by the slowest provider plus the judge. A remote provider
	// represents that entire two-stage server-side pipeline, so its outer client
	// deadline must cover both stages rather than one local provider attempt.
	phaseTimeout := buildPhaseAttemptTimeout(phase)
	if provider == "remote" {
		return 2*phaseTimeout + 15*time.Second
	}
	return phaseTimeout
}

func withProviderAttemptTimeoutFor(ctx context.Context, mode UsageMode, phase BuildPhase, provider string) (context.Context, context.CancelFunc) {
	maxDur := providerAttemptTimeout(mode, phase, provider)
	if maxDur <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) <= maxDur {
			return ctx, func() {}
		}
	}
	return context.WithTimeout(ctx, maxDur)
}

func withProviderAttemptTimeout(ctx context.Context, phase BuildPhase) (context.Context, context.CancelFunc) {
	// Backward-compatible helper for tests/default paths.
	return withProviderAttemptTimeoutFor(ctx, ModeBalanced, phase, "")
}
