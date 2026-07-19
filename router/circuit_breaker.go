package router

import (
	"fmt"
	"sync"
	"time"
)

const (
	defaultCircuitFailureThreshold = 3
	defaultCircuitCooldown         = 30 * time.Second
)

type breakerState struct {
	consecutiveFailures int
	openUntil           time.Time
	halfOpen            bool
	// halfOpenProbeAt records when the current half-open trial call was
	// admitted. While set, further BeforeAttempt calls are rejected so exactly
	// one concurrent probe decides the outcome; RecordSuccess/RecordFailure
	// clear it. A probe that never reports back (e.g. its caller was canceled)
	// expires after one cooldown so the provider cannot stay blocked forever.
	halfOpenProbeAt time.Time
}

// providerCircuitBreaker implements a per-provider circuit breaker with
// closed/open/half-open states.
type providerCircuitBreaker struct {
	mu               sync.Mutex
	now              func() time.Time
	failureThreshold int
	cooldown         time.Duration
	states           map[string]*breakerState
}

func newProviderCircuitBreaker(failureThreshold int, cooldown time.Duration) *providerCircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = defaultCircuitFailureThreshold
	}
	if cooldown <= 0 {
		cooldown = defaultCircuitCooldown
	}
	return &providerCircuitBreaker{
		now:              time.Now,
		failureThreshold: failureThreshold,
		cooldown:         cooldown,
		states:           make(map[string]*breakerState),
	}
}

// PeekOpen reports whether the provider circuit is currently open.
func (cb *providerCircuitBreaker) PeekOpen(provider string) (bool, time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.peekOpenLocked(provider)
}

// BeforeAttempt transitions from open->half-open when cooldown has elapsed.
// It returns false while the circuit is still open, and false for every caller
// but the first while a half-open trial call is in flight.
func (cb *providerCircuitBreaker) BeforeAttempt(provider string) (bool, time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state := cb.stateLocked(provider)
	now := cb.now()
	if state.openUntil.After(now) {
		return false, time.Until(state.openUntil)
	}
	if !state.openUntil.IsZero() && !state.openUntil.After(now) {
		// Cooldown elapsed: allow one trial call in half-open.
		state.openUntil = time.Time{}
		state.halfOpen = true
		state.halfOpenProbeAt = now
		state.consecutiveFailures = 0
		return true, 0
	}
	if state.halfOpen {
		// A half-open trial is already in flight: block concurrent callers so a
		// single probe decides the outcome (routing falls to the next candidate).
		// If the probe never reports back, re-arm after one cooldown.
		if elapsed := now.Sub(state.halfOpenProbeAt); elapsed < cb.cooldown {
			return false, cb.cooldown - elapsed
		}
		state.halfOpenProbeAt = now
	}
	return true, 0
}

func (cb *providerCircuitBreaker) RecordSuccess(provider string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state := cb.stateLocked(provider)
	state.consecutiveFailures = 0
	state.openUntil = time.Time{}
	state.halfOpen = false
	state.halfOpenProbeAt = time.Time{}
}

func (cb *providerCircuitBreaker) RecordFailure(provider string) (opened bool, until time.Time) {
	return cb.RecordFailureWeighted(provider, 1)
}

func (cb *providerCircuitBreaker) RecordFailureWeighted(provider string, weight int) (opened bool, until time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if weight <= 0 {
		weight = 1
	}

	state := cb.stateLocked(provider)
	now := cb.now()
	if state.halfOpen {
		state.halfOpen = false
		state.halfOpenProbeAt = time.Time{}
		state.consecutiveFailures = 0
		state.openUntil = now.Add(cb.cooldown)
		return true, state.openUntil
	}

	state.consecutiveFailures += weight
	if state.consecutiveFailures >= cb.failureThreshold {
		state.consecutiveFailures = 0
		state.openUntil = now.Add(cb.cooldown)
		state.halfOpen = false
		return true, state.openUntil
	}
	return false, time.Time{}
}

func (cb *providerCircuitBreaker) stateLocked(provider string) *breakerState {
	s, ok := cb.states[provider]
	if !ok {
		s = &breakerState{}
		cb.states[provider] = s
	}
	return s
}

func (cb *providerCircuitBreaker) peekOpenLocked(provider string) (bool, time.Duration) {
	s, ok := cb.states[provider]
	if !ok {
		return false, 0
	}
	now := cb.now()
	if s.openUntil.After(now) {
		return true, time.Until(s.openUntil)
	}
	return false, 0
}

func circuitOpenDetail(provider string, remaining time.Duration) string {
	if remaining <= 0 {
		return fmt.Sprintf("circuit open for %s", provider)
	}
	return fmt.Sprintf("circuit open for %s (retry in %s)", provider, remaining.Round(time.Second))
}
