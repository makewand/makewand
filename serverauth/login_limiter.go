package serverauth

import (
	"fmt"
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
	trusted     *TrustedProxies
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

// SetTrustedProxies configures the proxy peers whose forwarding headers are
// honored when deriving throttle keys. When unset (the default), forwarding
// headers are ignored and keys use the direct peer address.
func (l *LoginRateLimiter) SetTrustedProxies(trusted *TrustedProxies) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trusted = trusted
}

// ThrottleKey derives the limiter key for a request using this limiter's
// trusted-proxy configuration.
func (l *LoginRateLimiter) ThrottleKey(req *http.Request, principal string) string {
	var trusted *TrustedProxies
	if l != nil {
		l.mu.Lock()
		trusted = l.trusted
		l.mu.Unlock()
	}
	return strings.TrimSpace(strings.ToLower(principal)) + "|" + ClientIP(req, trusted)
}

// LoginThrottleKey derives a limiter key from the request's direct peer
// address. Client-supplied forwarding headers (X-Forwarded-For, X-Real-IP) are
// ignored; use LoginRateLimiter.ThrottleKey with SetTrustedProxies to honor
// them behind a trusted reverse proxy.
func LoginThrottleKey(req *http.Request, principal string) string {
	return strings.TrimSpace(strings.ToLower(principal)) + "|" + ClientIP(req, nil)
}

// TrustedProxies matches direct peer addresses against operator-configured
// reverse proxy networks.
type TrustedProxies struct {
	nets []*net.IPNet
}

// ParseTrustedProxies parses proxy addresses in CIDR notation (bare IPs are
// treated as /32 or /128). An empty list yields nil, meaning no proxies are
// trusted.
func ParseTrustedProxies(values []string) (*TrustedProxies, error) {
	nets := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if !strings.Contains(value, "/") {
			ip := net.ParseIP(value)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy address %q", value)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			value = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", value, err)
		}
		nets = append(nets, network)
	}
	if len(nets) == 0 {
		return nil, nil
	}
	return &TrustedProxies{nets: nets}, nil
}

// Trusts reports whether ip belongs to a configured trusted proxy network.
func (t *TrustedProxies) Trusts(ip net.IP) bool {
	if t == nil || ip == nil {
		return false
	}
	for _, network := range t.nets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the throttling address for a request. Forwarding headers
// are honored only when the direct peer is a trusted proxy; otherwise the
// direct peer address is used.
func ClientIP(req *http.Request, trusted *TrustedProxies) string {
	if req == nil {
		return ""
	}
	host := remoteHost(req.RemoteAddr)
	if !trusted.Trusts(net.ParseIP(host)) {
		return host
	}
	if forwarded := strings.TrimSpace(req.Header.Get("X-Forwarded-For")); forwarded != "" {
		if client := strings.TrimSpace(strings.Split(forwarded, ",")[0]); client != "" {
			return client
		}
	}
	if realIP := strings.TrimSpace(req.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	return host
}

func remoteHost(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || strings.TrimSpace(host) == "" {
		return remoteAddr
	}
	return strings.TrimSpace(host)
}
