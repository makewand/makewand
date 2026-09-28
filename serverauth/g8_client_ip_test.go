package serverauth

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func trustedSet(t *testing.T, values ...string) *TrustedProxies {
	t.Helper()
	tp, err := ParseTrustedProxies(values)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

// go-server#2: behind an appending reverse proxy the left-most X-Forwarded-For
// hop is client controlled. ClientIP must walk the chain from the right and
// return the first hop that is not a trusted proxy.
func TestG8_ClientIPUsesRightmostUntrustedForwardedHop(t *testing.T) {
	cases := []struct {
		name    string
		trusted []string
		peer    string
		xff     []string
		realIP  string
		want    string
	}{
		{"spoofed-first-hop", []string{"127.0.0.1"}, "127.0.0.1:4000", []string{"10.66.0.1, 203.0.113.7"}, "", "203.0.113.7"},
		{"multi-proxy-chain", []string{"127.0.0.1", "10.0.0.0/8"}, "127.0.0.1:4000", []string{"6.6.6.6, 198.51.100.9, 10.1.1.1"}, "", "198.51.100.9"},
		{"multiple-header-lines", []string{"127.0.0.1"}, "127.0.0.1:4000", []string{"6.6.6.6", "203.0.113.7"}, "", "203.0.113.7"},
		{"hop-with-port", []string{"127.0.0.1"}, "127.0.0.1:4000", []string{"6.6.6.6, 203.0.113.7:4444"}, "", "203.0.113.7"},
		{"ipv6-hop-with-port", []string{"127.0.0.1"}, "127.0.0.1:4000", []string{"6.6.6.6, [2001:db8::1]:443"}, "", "2001:db8::1"},
		{"all-hops-trusted", []string{"127.0.0.1", "10.0.0.0/8"}, "127.0.0.1:4000", []string{"10.1.1.1, 10.2.2.2"}, "", "10.1.1.1"},
		{"malformed-appended-hop-falls-back-to-peer", []string{"127.0.0.1"}, "127.0.0.1:4000", []string{"6.6.6.6, not-an-ip"}, "", "127.0.0.1"},
		{"x-real-ip-when-no-xff", []string{"127.0.0.1"}, "127.0.0.1:4000", nil, "203.0.113.8", "203.0.113.8"},
		{"invalid-x-real-ip-ignored", []string{"127.0.0.1"}, "127.0.0.1:4000", nil, "evil", "127.0.0.1"},
		{"untrusted-peer-ignores-headers", []string{"127.0.0.1"}, "192.0.2.10:4000", []string{"203.0.113.7"}, "203.0.113.8", "192.0.2.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/users/login", nil)
			req.RemoteAddr = tc.peer
			for _, value := range tc.xff {
				req.Header.Add("X-Forwarded-For", value)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}
			if got := ClientIP(req, trustedSet(t, tc.trusted...)); got != tc.want {
				t.Fatalf("ClientIP=%q, want %q", got, tc.want)
			}
		})
	}
}

// Rotating a forged left-most hop must not reset the login failure counter.
func TestG8_LoginLimiterCannotBeBypassedWithForgedForwardedFor(t *testing.T) {
	limiter := NewLoginRateLimiter(5, 15*time.Minute, 15*time.Minute)
	limiter.SetTrustedProxies(trustedSet(t, "127.0.0.1"))
	now := time.Now().UTC()
	blocked := false
	for i := 1; i <= 8; i++ {
		req := httptest.NewRequest("POST", "/v1/users/login", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.66.0.%d, 203.0.113.7", i))
		key := limiter.ThrottleKey(req, "dave@example.com")
		if allowed, _ := limiter.Allow(key, now); !allowed {
			blocked = true
			if i > 6 {
				t.Fatalf("lockout only after attempt %d", i)
			}
			break
		}
		limiter.RecordFailure(key, now)
	}
	if !blocked {
		t.Fatal("forged X-Forwarded-For hops bypassed the login limiter")
	}
}

func TestG8_RegistrationLimiterCannotBeBypassedWithForgedForwardedFor(t *testing.T) {
	limiter := NewRegistrationRateLimiter(1, 5, 1000, time.Hour)
	limiter.SetTrustedProxies(trustedSet(t, "127.0.0.1"))
	now := time.Now().UTC()
	allowed := 0
	for i := 1; i <= 40; i++ {
		req := httptest.NewRequest("POST", "/v1/users/register", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.66.%d.%d, 203.0.113.7", i/250, i%250))
		if limiter.AllowAt(ClientIP(req, limiter.TrustedProxies()), now) {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("forged hops allowed %d registrations from one client, want 5", allowed)
	}
}

// IPv6 clients usually control a whole /64, so per-source limits bucket it.
func TestG8_RateLimitSourceBucketsIPv6Slash64(t *testing.T) {
	if got := RateLimitSource("2001:db8:1:2::1"); got != RateLimitSource("2001:db8:1:2:ffff::9") {
		t.Fatalf("same /64 produced different buckets: %q", got)
	}
	if RateLimitSource("2001:db8:1:2::1") == RateLimitSource("2001:db8:1:3::1") {
		t.Fatal("different /64 networks share a bucket")
	}
	if got := RateLimitSource("203.0.113.7"); got != "203.0.113.7" {
		t.Fatalf("IPv4 bucket=%q", got)
	}
	if got := RateLimitSource("::ffff:203.0.113.7"); got != "203.0.113.7" {
		t.Fatalf("IPv4-mapped bucket=%q", got)
	}
	if got := RateLimitSource("not-an-ip"); got != "not-an-ip" {
		t.Fatalf("opaque bucket=%q", got)
	}

	limiter := NewRegistrationRateLimiter(1, 2, 1000, time.Hour)
	now := time.Now().UTC()
	allowed := 0
	for i := 1; i <= 10; i++ {
		if limiter.AllowAt(fmt.Sprintf("2001:db8:1:2::%x", i), now) {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("rotating addresses inside one /64 allowed %d registrations, want 2", allowed)
	}

	login := NewLoginRateLimiter(5, time.Minute, time.Minute)
	a := httptest.NewRequest("POST", "/", nil)
	a.RemoteAddr = "[2001:db8:1:2::1]:5000"
	b := httptest.NewRequest("POST", "/", nil)
	b.RemoteAddr = "[2001:db8:1:2::abcd]:5000"
	if login.ThrottleKey(a, "u") != login.ThrottleKey(b, "u") {
		t.Fatal("login throttle keys differ inside one /64")
	}
	if !strings.HasPrefix(login.ThrottleKey(a, "U"), "u|") {
		t.Fatalf("principal not normalized: %q", login.ThrottleKey(a, "U"))
	}
}

// go-server#3: the global cap is configurable (including off) and alerts the
// operator once per window instead of silently rejecting everyone.
func TestG8_RegistrationGlobalLimitConfigurableAndAlerts(t *testing.T) {
	now := time.Now().UTC()
	unlimited := NewRegistrationRateLimiterWithLimits(RegistrationLimits{MaxPerSource: 1, MaxGlobal: -1})
	for i := 0; i < 200; i++ {
		if !unlimited.AllowAt(fmt.Sprintf("198.51.%d.%d", i/200, i%200), now) {
			t.Fatalf("disabled global cap rejected source %d", i)
		}
	}

	limited := NewRegistrationRateLimiterWithLimits(RegistrationLimits{MaxPerSource: 1, MaxGlobal: 3, Window: time.Hour})
	var alerts []RegistrationLimitAlert
	limited.SetGlobalLimitAlert(func(alert RegistrationLimitAlert) { alerts = append(alerts, alert) })
	for i := 0; i < 6; i++ {
		limited.AllowAt(fmt.Sprintf("192.0.2.%d", i), now)
	}
	if len(alerts) != 1 || alerts[0].Limit != 3 || alerts[0].Window != time.Hour {
		t.Fatalf("alerts=%+v, want exactly one for limit 3", alerts)
	}
	// A new window resets both the counter and the alert latch.
	later := now.Add(2 * time.Hour)
	for i := 0; i < 6; i++ {
		limited.AllowAt(fmt.Sprintf("192.0.2.%d", i), later)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts after window reset=%d, want 2", len(alerts))
	}

	limits := limited.Limits()
	if limits.MaxGlobal != 3 || limits.MaxPerSource != 1 || limits.MaxConcurrent != 2 {
		t.Fatalf("limits=%+v", limits)
	}
}
