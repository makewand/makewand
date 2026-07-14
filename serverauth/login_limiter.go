package serverauth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type LoginRateLimiter struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	lockout     time.Duration
	attempts    map[string]loginAttempt
}

type loginAttempt struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

func NewLoginRateLimiter(maxFailures int, window, lockout time.Duration) *LoginRateLimiter {
	if maxFailures <= 0 {
		maxFailures = 5
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	if lockout <= 0 {
		lockout = 15 * time.Minute
	}
	return &LoginRateLimiter{
		maxFailures: maxFailures,
		window:      window,
		lockout:     lockout,
		attempts:    make(map[string]loginAttempt),
	}
}

func (l *LoginRateLimiter) Allow(key string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	key = strings.TrimSpace(strings.ToLower(key))
	if key == "" {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt, ok := l.attempts[key]
	if !ok {
		return true, 0
	}
	if !attempt.lockedUntil.IsZero() && now.Before(attempt.lockedUntil) {
		return false, attempt.lockedUntil.Sub(now)
	}
	if !attempt.windowStart.IsZero() && now.Sub(attempt.windowStart) > l.window {
		delete(l.attempts, key)
	}
	return true, 0
}

func (l *LoginRateLimiter) RecordFailure(key string, now time.Time) {
	if l == nil {
		return
	}
	key = strings.TrimSpace(strings.ToLower(key))
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt := l.attempts[key]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) > l.window {
		attempt = loginAttempt{windowStart: now}
	}
	attempt.failures++
	if attempt.failures >= l.maxFailures {
		attempt.lockedUntil = now.Add(l.lockout)
	}
	l.attempts[key] = attempt
}

func (l *LoginRateLimiter) Reset(key string) {
	if l == nil {
		return
	}
	key = strings.TrimSpace(strings.ToLower(key))
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

func LoginThrottleKey(req *http.Request, principal string) string {
	principal = strings.TrimSpace(strings.ToLower(principal))
	host := ""
	if req != nil {
		forwarded := strings.TrimSpace(req.Header.Get("X-Forwarded-For"))
		if forwarded != "" {
			host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
		if host == "" {
			host = strings.TrimSpace(req.Header.Get("X-Real-IP"))
		}
		if host == "" {
			host, _, _ = net.SplitHostPort(strings.TrimSpace(req.RemoteAddr))
			if host == "" {
				host = strings.TrimSpace(req.RemoteAddr)
			}
		}
	}
	return principal + "|" + host
}
