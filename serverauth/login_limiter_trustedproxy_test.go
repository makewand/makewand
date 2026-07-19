package serverauth

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustIP(t *testing.T, value string) net.IP {
	t.Helper()
	ip := net.ParseIP(value)
	if ip == nil {
		t.Fatalf("ParseIP(%q) = nil", value)
	}
	return ip
}

func TestLoginThrottleKey_IgnoresForwardHeadersByDefault(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/users/login", nil)
	req.RemoteAddr = "10.0.0.5:5555"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")

	key := LoginThrottleKey(req, "User@Example.com")
	if !strings.Contains(key, "10.0.0.5") {
		t.Fatalf("key %q should key on the direct peer address", key)
	}
	if strings.Contains(key, "1.2.3.4") || strings.Contains(key, "5.6.7.8") {
		t.Fatalf("key %q must ignore forwarding headers without a trusted proxy", key)
	}
	if !strings.HasPrefix(key, "user@example.com|") {
		t.Fatalf("key %q should be lowercased principal + host", key)
	}
}

func TestLoginRateLimiter_ThrottleKeyHonorsTrustedProxy(t *testing.T) {
	l := NewLoginRateLimiter(5, time.Minute, time.Minute)

	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "10.0.0.5:5555"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.5")

	// Without a configured trusted proxy the direct peer wins.
	if key := l.ThrottleKey(req, "u"); !strings.Contains(key, "10.0.0.5") || strings.Contains(key, "1.2.3.4") {
		t.Fatalf("default key %q must use the direct peer, not XFF", key)
	}

	tp, err := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	l.SetTrustedProxies(tp)

	// The peer is now trusted, so the first XFF hop is used.
	if key := l.ThrottleKey(req, "u"); !strings.Contains(key, "1.2.3.4") {
		t.Fatalf("trusted-proxy key %q must use the forwarded client IP", key)
	}

	// An untrusted peer must still ignore XFF even with a trusted set configured.
	untrusted := httptest.NewRequest("POST", "/", nil)
	untrusted.RemoteAddr = "203.0.113.9:5555"
	untrusted.Header.Set("X-Forwarded-For", "1.2.3.4")
	if key := l.ThrottleKey(untrusted, "u"); !strings.Contains(key, "203.0.113.9") || strings.Contains(key, "1.2.3.4") {
		t.Fatalf("untrusted-peer key %q must ignore XFF", key)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	if tp, err := ParseTrustedProxies(nil); err != nil || tp != nil {
		t.Fatalf("empty list = (%v,%v), want (nil,nil)", tp, err)
	}
	if _, err := ParseTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Fatal("invalid proxy address should error")
	}
	tp, err := ParseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if !tp.Trusts(mustIP(t, "127.0.0.1")) {
		t.Fatal("127.0.0.1 should be trusted as a /32")
	}
	if !tp.Trusts(mustIP(t, "10.5.5.5")) {
		t.Fatal("10.5.5.5 should be trusted by 10.0.0.0/8")
	}
	if tp.Trusts(mustIP(t, "192.168.1.1")) {
		t.Fatal("192.168.1.1 should not be trusted")
	}
}
