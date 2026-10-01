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
	// one concurrent probe decides the outcome; the probe's success or any
	// failure clears it. The owning attempt explicitly releases a canceled
	// lease; wall-clock cooldown never admits a second live probe.
	halfOpenProbeAt time.Time
	// epoch counts transitions into the open state. Admission tickets carry the
	// epoch they were issued in, so a success can be matched to the half-open
	// probe of the CURRENT open episode.
	epoch uint64
	lease uint64
}

// breakerTicket identifies one admission granted by Admit. A success only
// closes an open/half-open circuit when it comes from the half-open probe
// admitted in the current epoch; successes of requests admitted earlier (which
// may complete after a concurrent timeout tripped the breaker) are stale and
// cannot shorten the cooldown.
type breakerTicket struct {
	epoch uint64
	lease uint64
	probe bool
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
// but the first while a half-open trial call is in flight. Callers that report
// the outcome should use Admit so the success can be matched to its admission.
func (cb *providerCircuitBreaker) BeforeAttempt(provider string) (bool, time.Duration) {
	allow, remaining, _ := cb.Admit(provider)
	return allow, remaining
}

// Admit is BeforeAttempt returning the admission ticket to hand back to
// RecordAdmittedSuccess when the call succeeds.
func (cb *providerCircuitBreaker) Admit(provider string) (bool, time.Duration, breakerTicket) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state := cb.stateLocked(provider)
	now := cb.now()
	if state.openUntil.After(now) {
		return false, time.Until(state.openUntil), breakerTicket{}
	}
	if !state.openUntil.IsZero() && !state.openUntil.After(now) {
		// Cooldown elapsed: allow one trial call in half-open.
		state.openUntil = time.Time{}
		state.halfOpen = true
		state.halfOpenProbeAt = now
		state.consecutiveFailures = 0
		state.lease++
		return true, 0, breakerTicket{epoch: state.epoch, lease: state.lease, probe: true}
	}
	if state.halfOpen {
		if !state.halfOpenProbeAt.IsZero() {
			return false, cb.cooldown, breakerTicket{}
		}
		state.halfOpenProbeAt = now
		state.lease++
		return true, 0, breakerTicket{epoch: state.epoch, lease: state.lease, probe: true}
	}

	return true, 0, breakerTicket{epoch: state.epoch}
}

// ReleaseAdmission abandons a matching half-open lease without treating a
// cancellation or accounting refusal as provider health evidence.
func (cb *providerCircuitBreaker) ReleaseAdmission(provider string, ticket breakerTicket) {
	if !ticket.probe {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state := cb.stateLocked(provider)
	if state.halfOpen && state.epoch == ticket.epoch && state.lease == ticket.lease {
		state.halfOpenProbeAt = time.Time{}
	}
}

func (r *Router) releaseBreakerAdmission(provider string, ticket breakerTicket) {
	if r.breaker != nil {
		r.breaker.ReleaseAdmission(provider, ticket)
	}
}

// RecordSuccess records a success that cannot be matched to an admission. It
// is treated like a stale success: it resets the failure streak of a closed
// circuit but never closes an open or half-open one (only the half-open probe
// may do that — see RecordAdmittedSuccess).
func (cb *providerCircuitBreaker) RecordSuccess(provider string) {
	cb.RecordAdmittedSuccess(provider, breakerTicket{})
}

// RecordAdmittedSuccess records the success of a call admitted with ticket.
//   - Closed circuit: resets the consecutive-failure streak.
//   - Open circuit (cooling down): ignored — the success belongs to a request
//     admitted before the circuit opened and says nothing about recovery.
//   - Half-open: closes the circuit only for the probe admitted in the current
//     epoch; any other success is stale and leaves the probe in charge.
func (cb *providerCircuitBreaker) RecordAdmittedSuccess(provider string, ticket breakerTicket) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state := cb.stateLocked(provider)
	if !state.openUntil.IsZero() {
		return
	}
	if state.halfOpen {
		if !ticket.probe || ticket.epoch != state.epoch || ticket.lease != state.lease {
			return
		}
		state.halfOpen = false
		state.halfOpenProbeAt = time.Time{}
	}
	state.consecutiveFailures = 0
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
		state.epoch++
		return true, state.openUntil
	}

	state.consecutiveFailures += weight
	if state.consecutiveFailures >= cb.failureThreshold {
		state.consecutiveFailures = 0
		if state.openUntil.IsZero() {
			// closed -> open: a new episode; tickets issued before it are stale.
			state.epoch++
		}
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
