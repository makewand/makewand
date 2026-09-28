package serveradmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func adminBrowserLogin(t *testing.T, sessions *SessionManager) (*http.Cookie, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	sessions.HandleSessionLogin(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/session/login", bytes.NewBufferString(`{"email":"admin@example.com","password":"password123"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login=%d %s", rec.Code, rec.Body.String())
	}
	var body adminSessionLoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == adminSessionCookieName {
			return cookie, body.CSRFToken
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil, ""
}

func sessionAuthenticated(sessions *SessionManager, cookie *http.Cookie) bool {
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/session/me", nil)
	req.AddCookie(cookie)
	_, _, ok := sessions.Authenticate(req)
	return ok
}

// go-server#10: logging out must invalidate the cookie on the server, not only
// clear it in the browser that logged out.
func TestG8_AdminLogoutRevokesCookieServerSide(t *testing.T) {
	f := newAuthorizationFixture(t)
	sessions, err := NewSessionManager(f.users, []byte("0123456789abcdef0123456789abcdef"), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := adminBrowserLogin(t, sessions)
	if !sessionAuthenticated(sessions, cookie) {
		t.Fatal("fresh cookie rejected")
	}
	other, _ := adminBrowserLogin(t, sessions)

	logout := httptest.NewRequest(http.MethodPost, "/v1/admin/session/logout", nil)
	logout.AddCookie(cookie)
	logout.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	sessions.HandleSessionLogout(rec, logout)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout=%d %s", rec.Code, rec.Body.String())
	}
	if sessionAuthenticated(sessions, cookie) {
		t.Fatal("stolen copy of a logged-out cookie is still accepted")
	}
	if !sessionAuthenticated(sessions, other) {
		t.Fatal("logout revoked an unrelated session")
	}

	// The revoked cookie cannot drive the admin API either.
	handler := NewHandler(HandlerOptions{TokenManager: f.tokens, UserStore: f.users, TeamStore: f.teams, SessionMgr: sessions})
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin API with logged-out cookie=%d %s", rec.Code, rec.Body.String())
	}
}

// Sessions expire after an idle period even inside the absolute TTL; activity
// extends them.
func TestG8_AdminSessionIdleTimeout(t *testing.T) {
	f := newAuthorizationFixture(t)
	sessions, err := NewSessionManager(f.users, []byte("0123456789abcdef0123456789abcdef"), 12*time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sessions.IdleTimeout() != DefaultAdminSessionIdleTimeout {
		t.Fatalf("default idle timeout=%s", sessions.IdleTimeout())
	}
	sessions.SetIdleTimeout(30 * time.Minute)
	clock := time.Now().UTC()
	sessions.now = func() time.Time { return clock }

	active, _ := adminBrowserLogin(t, sessions)
	idle, _ := adminBrowserLogin(t, sessions)
	for i := 0; i < 4; i++ {
		clock = clock.Add(20 * time.Minute)
		if !sessionAuthenticated(sessions, active) {
			t.Fatalf("active session expired after %d idle-free intervals", i+1)
		}
	}
	if sessionAuthenticated(sessions, idle) {
		t.Fatal("idle session survived past the idle timeout")
	}
	clock = clock.Add(31 * time.Minute)
	if sessionAuthenticated(sessions, active) {
		t.Fatal("session survived 31 idle minutes")
	}
	sessions.mu.Lock()
	remaining := len(sessions.sessions)
	sessions.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired sessions retained in memory: %d", remaining)
	}
}
