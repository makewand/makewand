package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/serverauth"
)

func TestUserStore_CreateUserPersistsAndValidatesPassword(t *testing.T) {
	storeDir := t.TempDir()
	store := NewUserStore(storeDir)

	user, err := store.CreateUser("Person@Example.COM", "secret123")
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	if user.Email != "person@example.com" {
		t.Fatalf("Email = %q, want lowercase", user.Email)
	}
	if user.PasswordHash == "" || user.Salt == "" {
		t.Fatal("expected password hash and salt to be stored")
	}
	if user.PasswordHash == "secret123" {
		t.Fatal("password hash should not equal plaintext password")
	}
	if user.Role != UserRoleMember {
		t.Fatalf("Role = %q, want %q", user.Role, UserRoleMember)
	}

	persistedPath := filepath.Join(storeDir, "users.json")
	if _, err := os.Stat(persistedPath); err != nil {
		t.Fatalf("users.json missing: %v", err)
	}

	got, err := store.GetUserByEmail("PERSON@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail() error = %v", err)
	}
	if got.ID != user.ID {
		t.Fatalf("loaded user ID = %q, want %q", got.ID, user.ID)
	}
	if !got.ValidatePassword("secret123") {
		t.Fatal("ValidatePassword(correct) = false, want true")
	}
	if got.ValidatePassword("wrong-pass") {
		t.Fatal("ValidatePassword(wrong) = true, want false")
	}

	if _, err := store.CreateUser("person@example.com", "another123"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate CreateUser() error = %v, want ErrUserExists", err)
	}
	if _, err := store.GetUserByEmail("missing@example.com"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("GetUserByEmail(missing) error = %v, want ErrUserNotFound", err)
	}
}

func TestUserStore_ListUsersAndMutations(t *testing.T) {
	store := NewUserStore(t.TempDir())
	user, err := store.CreateUser("person@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.SetUserRole(user.ID, UserRoleAdmin); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}
	if _, err := store.SetUserActive(user.ID, false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}

	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("ListUsers() len = %d, want 1", len(users))
	}
	if users[0].Role != UserRoleAdmin {
		t.Fatalf("Role = %q, want %q", users[0].Role, UserRoleAdmin)
	}
	if users[0].IsActive {
		t.Fatal("IsActive = true, want false")
	}
}

func TestHTTPHandlerWithUsers_RegisterUserAndKeepModelAuth(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
	store := NewUserStore(t.TempDir())
	handler := r.HTTPHandlerWithUsers(store, HTTPHandlerOptions{BearerToken: "secret123"})

	body := `{"email":"User@example.com","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/users/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	var resp UserRegistrationResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode registration response: %v", err)
	}
	if resp.Email != "user@example.com" {
		t.Fatalf("response email = %q, want lowercase", resp.Email)
	}
	if resp.Role != UserRoleMember {
		t.Fatalf("response role = %q, want %q", resp.Role, UserRoleMember)
	}
	if resp.ID == "" {
		t.Fatal("response ID should not be empty")
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRec := httptest.NewRecorder()
	handler.ServeHTTP(modelsRec, modelsReq)
	if modelsRec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models status = %d, want 401", modelsRec.Code)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/v1/users/register", bytes.NewBufferString(body))
	dupReq.Header.Set("Content-Type", "application/json")
	dupRec := httptest.NewRecorder()
	handler.ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusConflict {
		t.Fatalf("duplicate register status = %d, want 409; body=%s", dupRec.Code, dupRec.Body.String())
	}
}

func TestHTTPHandlerWithUsers_RejectsInvalidRegistration(t *testing.T) {
	r := NewRouterFromConfig(RouterConfig{})
	handler := r.HTTPHandlerWithUsers(NewUserStore(t.TempDir()))

	req := httptest.NewRequest(http.MethodPost, "/v1/users/register", bytes.NewBufferString(`{"email":"bad-email","password":"short"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid register status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandlerWithUsers_LoginIssuesToken(t *testing.T) {
	stub := &stubProvider{name: "claude", available: true}
	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: stub, Access: AccessSubscription},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})

	userStore, err := OpenSQLiteUserStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteUserStore: %v", err)
	}
	defer userStore.Close()
	if _, err := userStore.CreateUserWithRole("admin@example.com", "password123", UserRoleAdmin); err != nil {
		t.Fatalf("CreateUserWithRole: %v", err)
	}

	tokenStore, err := serverauth.OpenSQLiteStore(filepath.Join(t.TempDir(), "tokens.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer tokenStore.Close()

	handler := r.HTTPHandlerWithUsers(userStore, HTTPHandlerOptions{UserTokenManager: tokenStore})
	req := httptest.NewRequest(http.MethodPost, "/v1/users/login", bytes.NewBufferString(`{"email":"admin@example.com","password":"password123"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp UserLoginResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if resp.Token == "" || resp.TokenID == "" {
		t.Fatalf("login response missing token fields: %+v", resp)
	}
	foundAdminScope := false
	for _, scope := range resp.Scopes {
		if scope == serverauth.ScopeAdminTokensRead {
			foundAdminScope = true
			break
		}
	}
	if !foundAdminScope {
		t.Fatalf("admin login scopes = %v, want admin scope", resp.Scopes)
	}
}
