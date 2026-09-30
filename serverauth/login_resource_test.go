package serverauth

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginAdmissionRotatingAccountsAndDistributedFailures(t *testing.T) {
	limiter := NewLoginRateLimiter(5, time.Minute, time.Minute)
	now := time.Now()
	req := httptest.NewRequest("POST", "/login", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	for i := 0; i < 30; i++ {
		if ok, _ := limiter.Admit(req, fmt.Sprintf("unknown%d@example.invalid", i), now); !ok {
			t.Fatalf("attempt %d rejected", i)
		}
	}
	if ok, _ := limiter.Admit(req, "new@example.invalid", now); ok {
		t.Fatal("rotating account bypassed source limit")
	}
	limiter = NewLoginRateLimiter(5, time.Minute, time.Minute)
	for i := 0; i < 5; i++ {
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
		if ok, _ := limiter.Admit(req, "target@example.invalid", now); !ok {
			t.Fatal("early lockout")
		}
		limiter.RecordFailure(limiter.ThrottleKey(req, "target@example.invalid"), now)
	}
	req.RemoteAddr = "192.0.2.100:1234"
	if ok, _ := limiter.Admit(req, "target@example.invalid", now); ok {
		t.Fatal("new address bypassed account lockout")
	}
}

func TestLoginAdmissionGlobalTTLAndCapacity(t *testing.T) {
	l := NewLoginRateLimiter(5, time.Minute, time.Minute)
	now := time.Now()
	req := httptest.NewRequest("POST", "/login", nil)
	for i := 0; i < 300; i++ {
		req.RemoteAddr = fmt.Sprintf("198.51.%d.%d:1234", i/250, i%250+1)
		if ok, _ := l.Admit(req, fmt.Sprint("account", i), now); !ok {
			t.Fatalf("attempt %d rejected", i)
		}
	}
	req.RemoteAddr = "203.0.113.1:1234"
	if ok, _ := l.Admit(req, "last", now); ok {
		t.Fatal("global limit bypassed")
	}
	for i := 0; i < 10000; i++ {
		l.RecordFailure(fmt.Sprintf("account%d|192.0.2.1", i), now)
	}
	if len(l.attempts) > maxLoginKeys {
		t.Fatal("failure state grew past capacity")
	}
	if ok, _ := l.Admit(req, "fresh", now.Add(2*time.Minute)); !ok {
		t.Fatal("expired limits remained active")
	}
	if len(l.attempts) != 0 {
		t.Fatal("expired unique account state was not reclaimed")
	}
}

func TestLoginAccountLockoutPreservesPrincipalSeparatorsAcrossIPs(t *testing.T) {
	for _, principal := range []string{"reader|tag@example.invalid", "Reader|More|Tags@example.invalid"} {
		t.Run(principal, func(t *testing.T) {
			limiter := NewLoginRateLimiter(5, 15*time.Minute, 15*time.Minute)
			now := time.Now()
			req := httptest.NewRequest("POST", "/v1/users/login", nil)
			for i := 0; i < 5; i++ {
				req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
				if ok, _ := limiter.Admit(req, principal, now); !ok {
					t.Fatalf("failure %d was rejected before account lockout", i+1)
				}
				limiter.RecordFailure(limiter.ThrottleKey(req, principal), now)
			}
			req.RemoteAddr = "198.51.100.1:5678"
			if ok, retryAfter := limiter.Admit(req, principal, now); ok || retryAfter <= 0 {
				t.Fatal("another source bypassed the complete principal's account lockout")
			}
			if ok, _ := limiter.Admit(req, "reader@example.invalid", now); !ok {
				t.Fatal("unrelated account was locked")
			}
			// A successful reset from a new source must clear that same complete
			// account bucket, even though this source has no pair failure entry.
			limiter.Reset(limiter.ThrottleKey(req, principal))
			if ok, _ := limiter.Admit(req, principal, now); !ok {
				t.Fatal("reset did not clear the complete principal's account lockout")
			}
		})
	}
}

func TestLoginAndRegistrationShareHashCapacity(t *testing.T) {
	ConfigurePasswordHashConcurrency(2)
	defer ConfigurePasswordHashConcurrency(DefaultRegistrationConcurrency)
	loginRelease, ok := AcquirePasswordHash()
	if !ok {
		t.Fatal("login slot rejected")
	}
	defer loginRelease()
	registration := NewRegistrationRateLimiter(4, 5, 30, time.Hour)
	registrationRelease, ok := registration.Acquire()
	if !ok {
		t.Fatal("registration slot rejected")
	}
	if release, ok := AcquirePasswordHash(); ok {
		release()
		t.Fatal("hash cap bypassed across endpoints")
	}
	registrationRelease()
	registrationRelease()
	if release, ok := AcquirePasswordHash(); !ok {
		t.Fatal("slot not released")
	} else {
		release()
	}
}
