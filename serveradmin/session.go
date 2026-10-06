package serveradmin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
)

const adminSessionCookieName = "makewand_admin_session"

// DefaultAdminSessionIdleTimeout ends a browser session that has not been used
// for this long, even inside its absolute lifetime.
const DefaultAdminSessionIdleTimeout = 30 * time.Minute

// SessionManager issues HMAC-signed admin browser cookies and tracks every live
// session in server memory. A cookie is accepted only while its session is
// registered, so logout revokes it on the server, idle sessions expire, and a
// server restart signs every browser session out.
type SessionManager struct {
	userStore   router.UserManager
	secret      []byte
	ttl         time.Duration
	idleTimeout time.Duration
	limiter     *serverauth.LoginRateLimiter
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]*adminSessionRecord
}

type adminSessionRecord struct {
	userID    string
	expiresAt time.Time
	lastSeen  time.Time
}

type sessionClaims struct {
	SessionID   string `json:"sid"`
	UserID      string `json:"user_id"`
	AuthVersion string `json:"auth_version"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	CSRFToken   string `json:"csrf_token"`
	ExpiresAt   int64  `json:"expires_at"`
}

type AdminSession struct {
	User      router.UserView `json:"user"`
	ExpiresAt time.Time       `json:"expires_at"`
	CSRFToken string          `json:"csrf_token"`
	id        string
}

type adminSessionLoginResponse struct {
	Authenticated bool            `json:"authenticated"`
	User          router.UserView `json:"user"`
	ExpiresAt     time.Time       `json:"expires_at"`
	CSRFToken     string          `json:"csrf_token"`
}

func NewSessionManager(userStore router.UserManager, secret []byte, ttl time.Duration, limiter *serverauth.LoginRateLimiter) (*SessionManager, error) {
	if userStore == nil {
		return nil, fmt.Errorf("user store is required")
	}
	secret = append([]byte(nil), secret...)
	if len(secret) < 32 {
		return nil, fmt.Errorf("admin session secret must be at least 32 bytes")
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionManager{
		userStore:   userStore,
		secret:      secret,
		ttl:         ttl,
		idleTimeout: DefaultAdminSessionIdleTimeout,
		limiter:     limiter,
		now:         func() time.Time { return time.Now().UTC() },
		sessions:    make(map[string]*adminSessionRecord),
	}, nil
}

// SetIdleTimeout changes the idle timeout; a non-positive value disables it
// (sessions then end only at their absolute expiry, logout, or restart).
func (m *SessionManager) SetIdleTimeout(timeout time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idleTimeout = timeout
}

// IdleTimeout returns the configured idle timeout.
func (m *SessionManager) IdleTimeout() time.Duration {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.idleTimeout
}

func (m *SessionManager) currentTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}

// touchSession reports whether sid is a live session for userID and, if so,
// records activity. Expired and idle sessions are removed.
func (m *SessionManager) touchSession(sid, userID string) bool {
	now := m.currentTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.sessions[sid]
	if !ok || record.userID != userID {
		return false
	}
	if !now.Before(record.expiresAt) || (m.idleTimeout > 0 && now.Sub(record.lastSeen) > m.idleTimeout) {
		delete(m.sessions, sid)
		return false
	}
	record.lastSeen = now
	return true
}

func (m *SessionManager) registerSession(sid, userID string, expiresAt time.Time) {
	now := m.currentTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, record := range m.sessions {
		if !now.Before(record.expiresAt) || (m.idleTimeout > 0 && now.Sub(record.lastSeen) > m.idleTimeout) {
			delete(m.sessions, id)
		}
	}
	m.sessions[sid] = &adminSessionRecord{userID: userID, expiresAt: expiresAt, lastSeen: now}
}

func (m *SessionManager) revokeSession(sid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sid)
}

func (m *SessionManager) HandleSessionLogin(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if m == nil || m.userStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "admin browser login is unavailable")
		return
	}
	var payload router.UserLoginRequest
	req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
	dec := newLimitedJSONDecoder(w, req)
	if err := dec.Decode(&payload); err != nil {
		status, code, message := adminJSONDecodeError(err)
		writeError(w, status, code, message)
		return
	}
	key := m.limiter.ThrottleKey(req, payload.Email)
	if len(payload.Password) > 1024 {
		writeError(w, http.StatusBadRequest, "invalid_request", "password is too long")
		return
	}
	if allowed, retryAfter := m.limiter.Admit(req, payload.Email, time.Now().UTC()); !allowed {
		writeError(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("too many failed admin logins; try again in %s", retryAfter.Round(time.Second)))
		return
	}
	releaseHash, acquired := serverauth.AcquirePasswordHash()
	if !acquired {
		writeError(w, http.StatusServiceUnavailable, "login_busy", "login is temporarily busy; try again shortly")
		return
	}
	defer releaseHash()
	user, err := m.userStore.GetUserByEmail(payload.Email)
	// Always spend one password hash: unknown and inactive accounts must take
	// as long as a wrong password, or response time reveals which accounts
	// exist.
	passwordOK := false
	if err == nil && user != nil {
		passwordOK = user.ValidatePassword(payload.Password)
	} else {
		_ = timingEqualizerUser.ValidatePassword(payload.Password)
	}
	if !passwordOK || !user.IsActive {
		m.limiter.RecordFailure(key, time.Now().UTC())
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid email or password")
		return
	}
	m.limiter.Reset(key)
	if !strings.EqualFold(user.Role, router.UserRoleAdmin) {
		writeError(w, http.StatusForbidden, "forbidden", "admin browser access requires an admin user")
		return
	}
	session, cookieValue, err := m.createSession(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login_failed", err.Error())
		return
	}
	m.setCookie(w, cookieValue, session.ExpiresAt)
	writeJSON(w, http.StatusOK, adminSessionLoginResponse{
		Authenticated: true,
		User:          session.User,
		ExpiresAt:     session.ExpiresAt,
		CSRFToken:     session.CSRFToken,
	})
}

func (m *SessionManager) HandleSessionLogout(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", "admin browser login is unavailable")
		return
	}
	if _, session, ok := m.Authenticate(req); ok {
		if requiresCSRFAuthorization(req.Method) && !m.ValidateCSRF(req, session.CSRFToken) {
			writeError(w, http.StatusForbidden, "forbidden", "missing or invalid CSRF token")
			return
		}
		// Revoke on the server so a copied cookie stops working too.
		m.revokeSession(session.id)
	}
	m.ClearCookie(w, req)
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

func (m *SessionManager) HandleSessionMe(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", "admin browser login is unavailable")
		return
	}
	_, session, ok := m.Authenticate(req)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, adminSessionLoginResponse{
		Authenticated: true,
		User:          session.User,
		ExpiresAt:     session.ExpiresAt,
		CSRFToken:     session.CSRFToken,
	})
}

func (m *SessionManager) Authenticate(req *http.Request) (*serverauth.Grant, *AdminSession, bool) {
	if m == nil || req == nil {
		return nil, nil, false
	}
	cookie, err := req.Cookie(adminSessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, nil, false
	}
	claims, ok := m.parseCookie(cookie.Value)
	if !ok || claims.SessionID == "" {
		return nil, nil, false
	}
	user, err := m.userStore.GetUserByID(claims.UserID)
	if err != nil || user == nil || !user.IsActive || !strings.EqualFold(user.Role, router.UserRoleAdmin) || !hmac.Equal([]byte(claims.AuthVersion), []byte(m.userAuthVersion(user))) {
		return nil, nil, false
	}
	if !m.touchSession(claims.SessionID, claims.UserID) {
		return nil, nil, false
	}
	grant, err := serverauth.GrantFromRule(serverauth.TokenRule{
		ID:          "websess_" + user.ID,
		Description: "admin browser session " + user.Email,
		Scopes:      serverauth.AllScopes(),
		ExpiresAt:   time.Unix(claims.ExpiresAt, 0).UTC(),
	})
	if err != nil {
		return nil, nil, false
	}
	session := &AdminSession{
		User:      user.View(),
		ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
		CSRFToken: claims.CSRFToken,
		id:        claims.SessionID,
	}
	return grant, session, true
}

func (m *SessionManager) ValidateCSRF(req *http.Request, expected string) bool {
	if m == nil {
		return false
	}
	got := strings.TrimSpace(req.Header.Get("X-CSRF-Token"))
	if got == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func (m *SessionManager) ClearCookie(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		Expires:  time.Unix(0, 0).UTC(),
		MaxAge:   -1,
	})
}

func (m *SessionManager) createSession(user *router.User) (*AdminSession, string, error) {
	if m == nil || user == nil {
		return nil, "", fmt.Errorf("admin session manager is unavailable")
	}
	expiresAt := m.currentTime().Add(m.ttl)
	claims := sessionClaims{
		SessionID:   randomToken(24),
		UserID:      user.ID,
		AuthVersion: m.userAuthVersion(user),
		Email:       user.Email,
		Role:        user.Role,
		CSRFToken:   randomToken(18),
		ExpiresAt:   expiresAt.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, "", err
	}
	sig := m.sign(payload)
	value := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
	m.registerSession(claims.SessionID, user.ID, time.Unix(claims.ExpiresAt, 0).UTC())
	return &AdminSession{
		User:      user.View(),
		ExpiresAt: expiresAt,
		CSRFToken: claims.CSRFToken,
		id:        claims.SessionID,
	}, value, nil
}

// Bind cookies to persisted authorization state without exposing password hashes.
// The HMAC changes on password, role, or active-state updates, survives restarts,
// and rejects cookies issued before authorization-version binding was introduced.
func (m *SessionManager) userAuthVersion(user *router.User) string {
	state, _ := json.Marshal([]any{user.ID, user.PasswordHash, user.Salt, user.Role, user.IsActive, user.UpdatedAt})
	return base64.RawURLEncoding.EncodeToString(m.sign(state))
}

func (m *SessionManager) parseCookie(value string) (*sessionClaims, bool) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != 2 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	expected := m.sign(payload)
	if subtle.ConstantTimeCompare(sig, expected) != 1 {
		return nil, false
	}
	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	if claims.UserID == "" || claims.CSRFToken == "" || claims.ExpiresAt == 0 {
		return nil, false
	}
	if m.currentTime().After(time.Unix(claims.ExpiresAt, 0).UTC()) {
		return nil, false
	}
	return &claims, true
}

func (m *SessionManager) setCookie(w http.ResponseWriter, value string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		Expires:  expiresAt.UTC(),
	})
}

func (m *SessionManager) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

// timingEqualizerUser has no valid password: validating against it performs
// the same Argon2id work as a real account and always fails.
var timingEqualizerUser = &router.User{Salt: "makewand-login-timing-equalizer"}

func randomToken(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func requiresCSRFAuthorization(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
