// quota_gate.go — how subscription quota influences routing.
//
// Two mechanisms, deliberately kept distinct (see the design discussion):
//
//   - Predicted headroom (from usage percentages) → SOFT band ranking. A pool
//     approaching its cap is sorted toward the back of the candidate list but is
//     never removed, so quota prediction alone can never cause a total routing
//     failure. This is the primary, everyday steering signal.
//
//   - Confirmed exhaustion (a provider returned a real quota/429 error) → HARD
//     block at the execution boundary (beforeProviderAttempt), sealed until the
//     window resets. Safe to hard-block because the pool is provably dead right
//     now; the seal's reset time gives inherent hysteresis.
//
// When no QuotaController is wired, every method here is a no-op and routing
// behaves exactly as it did before quota awareness.
package router

import "time"

// QuotaController is the router's view of the quota subsystem. *QuotaSnapshotter
// satisfies it; tests can supply a fake.
type QuotaController interface {
	Snapshot() *QuotaSnapshot
	MarkExhausted(provider string, until time.Time)
	Sealed(provider string) (time.Time, bool)
}

// quotaBandFor returns the soft-ranking band for a provider based on predicted
// headroom. Unknown providers and a nil controller rank as OK (neutral).
func (r *Router) quotaBandFor(provider string) QuotaBand {
	if r.quota == nil {
		return QuotaBandOK
	}
	q, ok := r.quota.Snapshot().Get(provider)
	if !ok {
		return QuotaBandOK
	}
	return r.quotaPolicy.band(q, false)
}

// quotaHardBlocked reports whether a provider is under an active confirmed-
// exhaustion seal, and until when.
func (r *Router) quotaHardBlocked(provider string) (bool, time.Time) {
	if r.quota == nil {
		return false, time.Time{}
	}
	if until, ok := r.quota.Sealed(provider); ok {
		return true, until
	}
	return false, time.Time{}
}

// noteQuotaError seals a provider when it returns a confirmed quota/rate-limit
// error, so subsequent attempts route elsewhere until the window resets. The
// reset time is taken from the latest snapshot when known, else a conservative
// default cooldown. Non-quota errors are ignored (left to the circuit breaker).
func (r *Router) noteQuotaError(provider string, err error) {
	if r.quota == nil || err == nil {
		return
	}
	if ErrorKindOf(err) != ErrorKindRateLimit {
		return
	}
	until := time.Now().Add(defaultQuotaSealCooldown)
	if q, ok := r.quota.Snapshot().Get(provider); ok && !q.ResetAt.IsZero() && q.ResetAt.After(time.Now()) {
		until = q.ResetAt
	}
	r.quota.MarkExhausted(provider, until)
}

// defaultQuotaSealCooldown is used when a quota error carries no known reset time.
const defaultQuotaSealCooldown = 30 * time.Minute
