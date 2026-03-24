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
	"time"

	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
)

const adminSessionCookieName = "makewand_admin_session"

type SessionManager struct {
	userStore router.UserManager
	secret    []byte
	ttl       time.Duration
	limiter   *serverauth.LoginRateLimiter
}

type sessionClaims struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CSRFToken string `json:"csrf_token"`
	ExpiresAt int64  `json:"expires_at"`
}

type AdminSession struct {
	User      router.UserView `json:"user"`
	ExpiresAt time.Time       `json:"expires_at"`
	CSRFToken string          `json:"csrf_token"`
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
		userStore: userStore,
		secret:    secret,
		ttl:       ttl,
		limiter:   limiter,
	}, nil
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
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	key := serverauth.LoginThrottleKey(req, payload.Email)
	if allowed, retryAfter := m.limiter.Allow(key, time.Now().UTC()); !allowed {
		writeError(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("too many failed admin logins; try again in %s", retryAfter.Round(time.Second)))
		return
	}
	user, err := m.userStore.GetUserByEmail(payload.Email)
	if err != nil || user == nil || !user.IsActive || !user.ValidatePassword(payload.Password) {
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
	m.setCookie(w, req, cookieValue, session.ExpiresAt)
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
	if !ok {
		return nil, nil, false
	}
	user, err := m.userStore.GetUserByID(claims.UserID)
	if err != nil || user == nil || !user.IsActive || !strings.EqualFold(user.Role, router.UserRoleAdmin) {
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

func (m *SessionManager) ClearCookie(w http.ResponseWriter, req *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestUsesTLS(req),
		Expires:  time.Unix(0, 0).UTC(),
		MaxAge:   -1,
	})
}

func (m *SessionManager) createSession(user *router.User) (*AdminSession, string, error) {
	if m == nil || user == nil {
		return nil, "", fmt.Errorf("admin session manager is unavailable")
	}
	expiresAt := time.Now().UTC().Add(m.ttl)
	claims := sessionClaims{
		UserID:    user.ID,
		Email:     user.Email,
		Role:      user.Role,
		CSRFToken: randomToken(18),
		ExpiresAt: expiresAt.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, "", err
	}
	sig := m.sign(payload)
	value := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
	return &AdminSession{
		User:      user.View(),
		ExpiresAt: expiresAt,
		CSRFToken: claims.CSRFToken,
	}, value, nil
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
	if time.Now().UTC().After(time.Unix(claims.ExpiresAt, 0).UTC()) {
		return nil, false
	}
	return &claims, true
}

func (m *SessionManager) setCookie(w http.ResponseWriter, req *http.Request, value string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestUsesTLS(req),
		Expires:  expiresAt.UTC(),
	})
}

func (m *SessionManager) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

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

func requestUsesTLS(req *http.Request) bool {
	if req == nil {
		return false
	}
	if req.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("X-Forwarded-Proto")), "https")
}
