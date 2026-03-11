// health.go — Circuit breaker wrappers and timeout management.
package router

import (
	"context"
	"errors"
	"strings"
	"time"
)

func (r *Router) isCircuitOpen(provider string) (bool, time.Duration) {
	if r.breaker == nil {
		return false, 0
	}
	return r.breaker.PeekOpen(provider)
}

func (r *Router) beforeProviderAttempt(provider string) (bool, time.Duration) {
	if r.breaker == nil {
		return true, 0
	}
	return r.breaker.BeforeAttempt(provider)
}

func (r *Router) recordProviderSuccess(provider string) {
	if r.breaker == nil {
		return
	}
	r.breaker.RecordSuccess(provider)
}

func (r *Router) recordProviderFailure(provider string) (bool, time.Time) {
	return r.recordProviderFailureForErr(provider, nil)
}

func (r *Router) recordProviderFailureForErr(provider string, callErr error) (bool, time.Time) {
	if r.breaker == nil {
		return false, time.Time{}
	}
	// Timeouts are usually transient provider saturation/outage signals.
	// Trip the breaker immediately so subsequent requests route elsewhere.
	if isTimeoutErr(callErr) {
		return r.breaker.RecordFailureWeighted(provider, defaultCircuitFailureThreshold)
	}
	return r.breaker.RecordFailure(provider)
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
			return 45 * time.Second
		default:
			return 60 * time.Second
		}
	}

	// Power mode: generous budgets — ensemble calls are parallel so total wall
	// time is bounded by the slowest provider, not the sum.
	return buildPhaseAttemptTimeout(phase)
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

func withStreamAttemptCancel(ch <-chan StreamChunk, cancel context.CancelFunc) <-chan StreamChunk {
	if ch == nil || cancel == nil {
		return ch
	}

	out := make(chan StreamChunk, 1)
	go func() {
		defer close(out)
		defer cancel()
		for chunk := range ch {
			select {
			case out <- chunk:
			case <-time.After(30 * time.Second):
				// Consumer stopped reading — abandon to avoid goroutine leak.
				return
			}
			if chunk.Done || chunk.Error != nil {
				return
			}
		}
	}()

	return out
}
