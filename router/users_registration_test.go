package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/makewand/makewand/serverauth"
)

func TestHTTPHandlerWithUsers_RegistrationDisabledByDefault(t *testing.T) {
	r := mustNewRouter(RouterConfig{})
	store := NewUserStore(t.TempDir())
	// Registration not enabled: the /v1/users/register route must not be mounted.
	handler := r.HTTPHandlerWithUsers(store, UserEndpointOptions{})

	req := httptest.NewRequest(http.MethodPost, "/v1/users/register", bytes.NewBufferString(`{"email":"a@example.com","password":"password123"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("register status = %d, want 404 when registration disabled; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := store.GetUserByEmail("a@example.com"); err == nil {
		t.Fatal("user was created despite registration being disabled")
	}
}

func TestHTTPHandlerWithUsers_RegistrationInactiveByDefault(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true}
	r := mustNewRouter(RouterConfig{
		Providers:    map[string]ProviderEntry{"claude": {Provider: stub, Access: AccessSubscription}},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	store := NewUserStore(t.TempDir())
	tokenStore, err := serverauth.OpenSQLiteStore(filepath.Join(t.TempDir(), "tokens.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer tokenStore.Close()

	handler := r.HTTPHandlerWithUsers(store, UserEndpointOptions{
		EnableRegistration:  true,
		RegistrationLimiter: serverauth.NewRegistrationRateLimiter(4, 10, 100, time.Hour),
	}, HTTPHandlerOptions{
		UserTokenManager: tokenStore,
		UserLoginLimiter: serverauth.NewLoginRateLimiter(5, time.Minute, time.Minute),
	})

	regReq := httptest.NewRequest(http.MethodPost, "/v1/users/register", bytes.NewBufferString(`{"email":"new@example.com","password":"password123"}`))
	regReq.Header.Set("Content-Type", "application/json")
	regRec := httptest.NewRecorder()
	handler.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want 201; body=%s", regRec.Code, regRec.Body.String())
	}

	// The account must be created inactive (awaiting admin activation).
	user, err := store.GetUserByEmail("new@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user.IsActive {
		t.Fatal("self-registered account should be inactive by default")
	}

	// Login must be denied for the inactive account.
	loginReq := httptest.NewRequest(http.MethodPost, "/v1/users/login", bytes.NewBufferString(`{"email":"new@example.com","password":"password123"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401 for inactive account; body=%s", loginRec.Code, loginRec.Body.String())
	}
}

func TestHTTPHandlerWithUsers_RegistrationRejectedWhenBusy(t *testing.T) {
	r := mustNewRouter(RouterConfig{})
	store := NewUserStore(t.TempDir())
	limiter := serverauth.NewRegistrationRateLimiter(1, 10, 100, time.Hour)
	// Hold the only concurrency slot so registration reports it is busy.
	release, ok := limiter.Acquire()
	if !ok {
		t.Fatal("failed to pre-acquire concurrency slot")
	}
	defer release()

	handler := r.HTTPHandlerWithUsers(store, UserEndpointOptions{
		EnableRegistration:  true,
		RegistrationLimiter: limiter,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/users/register", bytes.NewBufferString(`{"email":"busy@example.com","password":"password123"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("register status = %d, want 503 when hashing slots exhausted; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, err := store.GetUserByEmail("busy@example.com"); err == nil {
		t.Fatal("user created despite concurrency limiter rejection")
	}
}
