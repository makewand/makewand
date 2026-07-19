package serverauth

import (
	"sync"
	"time"
)

// RegistrationRateLimiter throttles self-service account registration. It
// bounds the number of concurrent password-hashing operations (Argon2id is
// deliberately expensive) and applies fixed-window per-IP and global rate
// limits.
type RegistrationRateLimiter struct {
	mu            sync.Mutex
	sem           chan struct{}
	maxPerIP      int
	maxGlobal     int
	window        time.Duration
	windowStart   time.Time
	globalCount   int
	perIPCounts   map[string]int
	trusted       *TrustedProxies
	trustedLoaded bool
}

// NewRegistrationRateLimiter creates a limiter allowing maxConcurrent
// simultaneous hashing operations, maxPerIP registrations per source address
// per window, and maxGlobal registrations per window. Non-positive values fall
// back to conservative defaults.
func NewRegistrationRateLimiter(maxConcurrent, maxPerIP, maxGlobal int, window time.Duration) *RegistrationRateLimiter {
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	if maxPerIP <= 0 {
		maxPerIP = 5
	}
	if maxGlobal <= 0 {
		maxGlobal = 30
	}
	if window <= 0 {
		window = time.Hour
	}
	return &RegistrationRateLimiter{
		sem:         make(chan struct{}, maxConcurrent),
		maxPerIP:    maxPerIP,
		maxGlobal:   maxGlobal,
		window:      window,
		perIPCounts: make(map[string]int),
	}
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
// supplied time. It returns false when the per-IP or global window limit is
// exhausted.
func (l *RegistrationRateLimiter) AllowAt(sourceIP string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windowStart.IsZero() || now.Sub(l.windowStart) > l.window {
		l.windowStart = now
		l.globalCount = 0
		l.perIPCounts = make(map[string]int)
	}
	if l.globalCount >= l.maxGlobal {
		return false
	}
	if l.perIPCounts[sourceIP] >= l.maxPerIP {
		return false
	}
	l.globalCount++
	l.perIPCounts[sourceIP]++
	return true
}

// Acquire reserves one concurrent hashing slot without blocking. It returns a
// release function and true on success, or false when all slots are busy.
func (l *RegistrationRateLimiter) Acquire() (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	select {
	case l.sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-l.sem })
		}, true
	default:
		return nil, false
	}
}
