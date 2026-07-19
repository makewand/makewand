package serverauth

import (
	"testing"
	"time"
)

func TestRegistrationRateLimiter_PerIPAndGlobalLimits(t *testing.T) {
	l := NewRegistrationRateLimiter(1, 2, 3, time.Hour)
	now := time.Now().UTC()

	if !l.AllowAt("1.2.3.4", now) {
		t.Fatal("first per-IP attempt should be allowed")
	}
	if !l.AllowAt("1.2.3.4", now) {
		t.Fatal("second per-IP attempt should be allowed")
	}
	if l.AllowAt("1.2.3.4", now) {
		t.Fatal("third attempt from same IP should be blocked by per-IP limit")
	}

	// Global budget is 3; two already consumed. One more IP is allowed, next blocked.
	if !l.AllowAt("5.6.7.8", now) {
		t.Fatal("third global attempt should be allowed")
	}
	if l.AllowAt("9.9.9.9", now) {
		t.Fatal("fourth global attempt should be blocked by global limit")
	}

	// Window reset re-opens the budget.
	if !l.AllowAt("1.2.3.4", now.Add(2*time.Hour)) {
		t.Fatal("attempt after window reset should be allowed")
	}
}

func TestRegistrationRateLimiter_AcquireBoundsConcurrency(t *testing.T) {
	l := NewRegistrationRateLimiter(1, 100, 100, time.Hour)

	release, ok := l.Acquire()
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	if _, ok := l.Acquire(); ok {
		t.Fatal("second concurrent acquire should fail while slot is held")
	}
	release()
	release() // idempotent release must not panic or over-release
	next, ok := l.Acquire()
	if !ok {
		t.Fatal("acquire after release should succeed")
	}
	next()
}

func TestRegistrationRateLimiter_NilIsPermissive(t *testing.T) {
	var l *RegistrationRateLimiter
	if !l.AllowAt("1.2.3.4", time.Now()) {
		t.Fatal("nil limiter AllowAt should be permissive")
	}
	release, ok := l.Acquire()
	if !ok {
		t.Fatal("nil limiter Acquire should succeed")
	}
	release()
	if l.TrustedProxies() != nil {
		t.Fatal("nil limiter TrustedProxies should be nil")
	}
}
