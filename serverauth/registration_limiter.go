package serverauth

import (
	"sync"
	"time"
)

// Default registration limits used when a limit is left at zero.
const (
	DefaultRegistrationConcurrency = 2
	DefaultRegistrationPerSource   = 5
	DefaultRegistrationGlobal      = 30
	DefaultRegistrationWindow      = time.Hour
)

// RegistrationLimits configures a RegistrationRateLimiter.
//
// The per-source limit is the primary control: every client address (IPv6
// bucketed by /64, see RateLimitSource) gets MaxPerSource registrations per
// Window. The global cap is a backstop that bounds how many pending (inactive)
// accounts can be created per Window in total; a negative MaxGlobal disables
// it. Zero values fall back to the defaults above.
type RegistrationLimits struct {
	MaxConcurrent int
	MaxPerSource  int
	MaxGlobal     int
	Window        time.Duration
}

// RegistrationLimitAlert describes the first global-cap rejection in a window.
type RegistrationLimitAlert struct {
	WindowStart time.Time
	Window      time.Duration
	Limit       int
	Source      string
}

// RegistrationRateLimiter throttles self-service account registration. It
// bounds the number of concurrent password-hashing operations (Argon2id is
// deliberately expensive) and applies fixed-window per-source and (optional)
// global rate limits.
type RegistrationRateLimiter struct {
	mu            sync.Mutex
	sem           chan struct{}
	limits        RegistrationLimits
	windowStart   time.Time
	globalCount   int
	perIPCounts   map[string]int
	globalAlerted bool
	onGlobalLimit func(RegistrationLimitAlert)
	trusted       *TrustedProxies
	trustedLoaded bool
}

// NewRegistrationRateLimiter creates a limiter allowing maxConcurrent
// simultaneous hashing operations, maxPerIP registrations per source address
// per window, and maxGlobal registrations per window. Non-positive values fall
// back to conservative defaults.
func NewRegistrationRateLimiter(maxConcurrent, maxPerIP, maxGlobal int, window time.Duration) *RegistrationRateLimiter {
	if maxGlobal < 0 {
		maxGlobal = 0
	}
	return NewRegistrationRateLimiterWithLimits(RegistrationLimits{
		MaxConcurrent: maxConcurrent,
		MaxPerSource:  maxPerIP,
		MaxGlobal:     maxGlobal,
		Window:        window,
	})
}

// NewRegistrationRateLimiterWithLimits creates a limiter from limits. Zero
// values use the defaults; a negative MaxGlobal disables the global cap.
func NewRegistrationRateLimiterWithLimits(limits RegistrationLimits) *RegistrationRateLimiter {
	if limits.MaxConcurrent <= 0 {
		limits.MaxConcurrent = DefaultRegistrationConcurrency
	}
	if limits.MaxPerSource <= 0 {
		limits.MaxPerSource = DefaultRegistrationPerSource
	}
	if limits.MaxGlobal == 0 {
		limits.MaxGlobal = DefaultRegistrationGlobal
	}
	if limits.MaxGlobal < 0 {
		limits.MaxGlobal = -1
	}
	if limits.Window <= 0 {
		limits.Window = DefaultRegistrationWindow
	}
	return &RegistrationRateLimiter{
		sem:         make(chan struct{}, limits.MaxConcurrent),
		limits:      limits,
		perIPCounts: make(map[string]int),
	}
}

// Limits returns the effective limits (MaxGlobal is -1 when disabled).
func (l *RegistrationRateLimiter) Limits() RegistrationLimits {
	if l == nil {
		return RegistrationLimits{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limits
}

// SetGlobalLimitAlert registers fn to be called (outside the limiter lock) the
// first time the global cap rejects a registration in each window, so the
// operator learns that legitimate sign-ups may be blocked.
func (l *RegistrationRateLimiter) SetGlobalLimitAlert(fn func(RegistrationLimitAlert)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onGlobalLimit = fn
}

// SetTrustedProxies configures the proxy peers whose forwarding headers are
// honored when deriving the source address. Default: headers are ignored.
func (l *RegistrationRateLimiter) SetTrustedProxies(trusted *TrustedProxies) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trusted = trusted
	l.trustedLoaded = true
}

// TrustedProxies returns the configured trusted proxy set, if any.
func (l *RegistrationRateLimiter) TrustedProxies() *TrustedProxies {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.trusted
}

// AllowAt consumes one registration attempt for the source address at the
// supplied time. It returns false when the per-source or global window limit
// is exhausted. Source addresses are bucketed with RateLimitSource.
func (l *RegistrationRateLimiter) AllowAt(sourceIP string, now time.Time) bool {
	if l == nil {
		return true
	}
	source := RateLimitSource(sourceIP)
	var alert *RegistrationLimitAlert
	var notify func(RegistrationLimitAlert)
	allowed := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.windowStart.IsZero() || now.Sub(l.windowStart) > l.limits.Window {
			l.windowStart = now
			l.globalCount = 0
			l.globalAlerted = false
			l.perIPCounts = make(map[string]int)
		}
		// The per-source limit is checked first so a source that already used
		// its allowance cannot trip the global backstop or its alert.
		if l.perIPCounts[source] >= l.limits.MaxPerSource {
			return false
		}
		if _, exists := l.perIPCounts[source]; !exists && len(l.perIPCounts) >= maxLoginKeys {
			return false
		}
		if l.limits.MaxGlobal > 0 && l.globalCount >= l.limits.MaxGlobal {
			if !l.globalAlerted && l.onGlobalLimit != nil {
				l.globalAlerted = true
				alert = &RegistrationLimitAlert{WindowStart: l.windowStart, Window: l.limits.Window, Limit: l.limits.MaxGlobal, Source: source}
				notify = l.onGlobalLimit
			}
			return false
		}
		l.globalCount++
		l.perIPCounts[source]++
		return true
	}()
	if alert != nil && notify != nil {
		notify(*alert)
	}
	return allowed
}

// Acquire reserves one concurrent hashing slot without blocking. It returns a
// release function and true on success, or false when all slots are busy.
func (l *RegistrationRateLimiter) Acquire() (func(), bool) {
	sharedRelease, ok := AcquirePasswordHash()
	if !ok {
		return nil, false
	}
	if l == nil {
		return sharedRelease, true
	}
	select {
	case l.sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-l.sem; sharedRelease() })
		}, true
	default:
		sharedRelease()
		return nil, false
	}
}
