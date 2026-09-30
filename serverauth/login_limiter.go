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
	admissions  map[string]loginAdmission
	lastCleanup time.Time
}

const maxLoginKeys = 4096

type loginAdmission struct {
	start time.Time
	count int
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
		admissions:  make(map[string]loginAdmission),
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
	l.cleanupLocked(now)
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
	l.cleanupLocked(now)
	for _, failureKey := range []string{key, "account:" + loginPrincipalFromKey(key)} {
		if _, exists := l.attempts[failureKey]; !exists && len(l.attempts) >= maxLoginKeys {
			continue
		}
		attempt := l.attempts[failureKey]
		if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) > l.window {
			attempt = loginAttempt{windowStart: now}
		}
		attempt.failures++
		if attempt.failures >= l.maxFailures {
			attempt.lockedUntil = now.Add(l.lockout)
		}
		l.attempts[failureKey] = attempt
	}
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
	delete(l.attempts, "account:"+loginPrincipalFromKey(key))
}

// The source address is appended last and cannot contain '|'. Principals can
// contain it, so the last separator preserves the complete account identity.
// Legacy caller keys without a source separator retain their existing meaning.
func loginPrincipalFromKey(key string) string {
	if separator := strings.LastIndexByte(key, '|'); separator >= 0 {
		return key[:separator]
	}
	return key
}

// Admit reserves a login attempt before any password hash. Source and global
// limits prevent rotating principals from bypassing account lockouts. Capacity
// exhaustion fails closed rather than evicting a live security limit.
func (l *LoginRateLimiter) Admit(req *http.Request, principal string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	principal = strings.ToLower(strings.TrimSpace(principal))
	if len(principal) > 254 {
		return false, l.window
	}
	source := RateLimitSource(ClientIP(req, l.TrustedProxies()))
	key := principal + "|" + source
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(now)
	for _, failureKey := range []string{key, "account:" + principal} {
		if attempt := l.attempts[failureKey]; now.Before(attempt.lockedUntil) {
			return false, attempt.lockedUntil.Sub(now)
		}
	}
	limits := []struct {
		key string
		max int
	}{{"source:" + source, 30}, {"global", 300}}
	for _, limit := range limits {
		bucket, exists := l.admissions[limit.key]
		if !exists && len(l.admissions) >= maxLoginKeys {
			return false, l.window
		}
		if !bucket.start.IsZero() && now.Sub(bucket.start) < l.window && bucket.count >= limit.max {
			return false, l.window - now.Sub(bucket.start)
		}
	}
	for _, limit := range limits {
		bucket := l.admissions[limit.key]
		if bucket.start.IsZero() || now.Sub(bucket.start) >= l.window {
			bucket = loginAdmission{start: now}
		}
		bucket.count++
		l.admissions[limit.key] = bucket
	}
	return true, 0
}

func (l *LoginRateLimiter) cleanupLocked(now time.Time) {
	if !l.lastCleanup.IsZero() && now.Sub(l.lastCleanup) < time.Minute {
		return
	}
	l.lastCleanup = now
	for key, attempt := range l.attempts {
		if now.Sub(attempt.windowStart) >= l.window && !now.Before(attempt.lockedUntil) {
			delete(l.attempts, key)
		}
	}
	for key, bucket := range l.admissions {
		if now.Sub(bucket.start) >= l.window {
			delete(l.admissions, key)
		}
	}
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
// trusted-proxy configuration. The address part is bucketed with
// RateLimitSource so rotating addresses inside one IPv6 /64 shares a key.
func (l *LoginRateLimiter) ThrottleKey(req *http.Request, principal string) string {
	return strings.TrimSpace(strings.ToLower(principal)) + "|" + RateLimitSource(ClientIP(req, l.TrustedProxies()))
}

// TrustedProxies returns the configured trusted proxy set, if any.
func (l *LoginRateLimiter) TrustedProxies() *TrustedProxies {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.trusted
}

// LoginThrottleKey derives a limiter key from the request's direct peer
// address. Client-supplied forwarding headers (X-Forwarded-For, X-Real-IP) are
// ignored; use LoginRateLimiter.ThrottleKey with SetTrustedProxies to honor
// them behind a trusted reverse proxy.
func LoginThrottleKey(req *http.Request, principal string) string {
	return strings.TrimSpace(strings.ToLower(principal)) + "|" + RateLimitSource(ClientIP(req, nil))
}

// RateLimitSource maps a client address to its rate-limiting bucket. IPv4
// addresses (including IPv4-mapped IPv6) are used as-is; IPv6 addresses are
// bucketed by their /64 network because a single client normally controls a
// whole /64 and could otherwise rotate addresses to evade per-source limits.
// Values that are not IP addresses are returned unchanged.
func RateLimitSource(addr string) string {
	addr = strings.TrimSpace(addr)
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return (&net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
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
//
// Behind trusted proxies the X-Forwarded-For chain (all header lines, in
// order) is walked from the right: hops appended by trusted proxies are
// skipped and the first untrusted hop is the client. Hops further left are
// supplied by the client and are never used, so a forged left-most hop cannot
// change the result. A malformed hop inside the proxy-appended part makes the
// chain untrustworthy and falls back to the direct peer. When every hop is a
// trusted proxy the left-most hop is used. X-Real-IP is consulted only when no
// X-Forwarded-For header is present and must be a valid IP address. Reverse
// proxies must therefore append to (or overwrite) X-Forwarded-For.
func ClientIP(req *http.Request, trusted *TrustedProxies) string {
	if req == nil {
		return ""
	}
	host := remoteHost(req.RemoteAddr)
	if !trusted.Trusts(net.ParseIP(host)) {
		return host
	}
	if values := req.Header.Values("X-Forwarded-For"); len(values) > 0 {
		var hops []string
		for _, value := range values {
			for _, hop := range strings.Split(value, ",") {
				if hop = strings.TrimSpace(hop); hop != "" {
					hops = append(hops, hop)
				}
			}
		}
		if len(hops) > 0 {
			var leftmost net.IP
			for i := len(hops) - 1; i >= 0; i-- {
				ip := parseForwardedIP(hops[i])
				if ip == nil {
					return host
				}
				if !trusted.Trusts(ip) {
					return ip.String()
				}
				leftmost = ip
			}
			return leftmost.String()
		}
	}
	if realIP := parseForwardedIP(req.Header.Get("X-Real-IP")); realIP != nil {
		return realIP.String()
	}
	return host
}

// parseForwardedIP parses one forwarding-header hop, accepting bare IPv4/IPv6
// addresses and host:port / [v6]:port forms. It returns nil for anything else.
func parseForwardedIP(value string) net.IP {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if ip := net.ParseIP(strings.Trim(value, "[]")); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return net.ParseIP(host)
	}
	return nil
}

func remoteHost(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || strings.TrimSpace(host) == "" {
		return remoteAddr
	}
	return strings.TrimSpace(host)
}
