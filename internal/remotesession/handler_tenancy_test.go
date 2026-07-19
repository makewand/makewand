package remotesession

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/makewand/makewand/serverauth"
)

// sessionAuthzForUser builds an authorizer whose token carries the given user
// identity and full client session scopes.
func sessionAuthzForUser(t *testing.T, token, userID string) serverauth.RequestAuthorizer {
	t.Helper()
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:     "tok_" + userID,
				Token:  token,
				UserID: userID,
				Scopes: serverauth.AllClientScopes(),
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer(%s): %v", userID, err)
	}
	return authz
}

func doSession(t *testing.T, handler http.Handler, method, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Buffer
	if body != "" {
		reader = bytes.NewBufferString(body)
	} else {
		reader = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, "/v1/sessions/shared-workspace", reader)
	req.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestHandler_TenantIsolatesSessionsByOwner verifies that two users sharing the
// same workspace ID cannot read, overwrite, or delete each other's sessions.
func TestHandler_TenantIsolatesSessionsByOwner(t *testing.T) {
	store := NewStore(t.TempDir())

	handlerA := NewHandlerWithAuthorizer(store, sessionAuthzForUser(t, "token-a", "usr_alice"))
	handlerB := NewHandlerWithAuthorizer(store, sessionAuthzForUser(t, "token-b", "usr_bob"))

	// Alice writes to "shared-workspace".
	if rec := doSession(t, handlerA, http.MethodPut, "token-a", `{"owner":"alice"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("alice PUT status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	// Bob, using the same workspace ID, must not see Alice's data.
	rec := doSession(t, handlerB, http.MethodGet, "token-b", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob GET status = %d, want 404 (isolated); body=%s", rec.Code, rec.Body.String())
	}

	// Bob writes his own data to the same workspace ID.
	if rec := doSession(t, handlerB, http.MethodPut, "token-b", `{"owner":"bob"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("bob PUT status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	// Alice's data must be untouched by Bob's write.
	recA := doSession(t, handlerA, http.MethodGet, "token-a", "")
	if recA.Code != http.StatusOK {
		t.Fatalf("alice GET status = %d, want 200; body=%s", recA.Code, recA.Body.String())
	}
	if got := recA.Body.String(); got != `{"owner":"alice"}` {
		t.Fatalf("alice data = %q, want alice payload (bob overwrote it)", got)
	}

	// Bob deletes "his" session; Alice's must survive.
	if rec := doSession(t, handlerB, http.MethodDelete, "token-b", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("bob DELETE status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	recA = doSession(t, handlerA, http.MethodGet, "token-a", "")
	if recA.Code != http.StatusOK {
		t.Fatalf("alice GET after bob delete = %d, want 200 (survived); body=%s", recA.Code, recA.Body.String())
	}
}

// TestHandler_NoIdentityPreservesLegacyKeys verifies that when the grant carries
// no identity (single-user/legacy operator token), stored keys are unchanged so
// existing sessions still resolve.
func TestHandler_NoIdentityPreservesLegacyKeys(t *testing.T) {
	store := NewStore(t.TempDir())
	// Pre-seed a session under the bare workspace ID (pre-tenancy layout).
	if err := store.Save("shared-workspace", []byte(`{"legacy":true}`)); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	// A full-access token with no user/org/project (legacy single-token style).
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{{Token: "legacy", Scopes: serverauth.AllScopes()}},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	handler := NewHandlerWithAuthorizer(store, authz)

	rec := doSession(t, handler, http.MethodGet, "legacy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"legacy":true}` {
		t.Fatalf("legacy data = %q, want pre-seeded payload", got)
	}
}

func TestSessionOwnerKey(t *testing.T) {
	if got := sessionOwnerKey(nil); got != "" {
		t.Fatalf("sessionOwnerKey(nil) = %q, want empty", got)
	}
	empty, err := serverauth.GrantFromRule(serverauth.TokenRule{Token: "t", Scopes: serverauth.AllClientScopes()})
	if err != nil {
		t.Fatalf("GrantFromRule: %v", err)
	}
	if got := sessionOwnerKey(empty); got != "" {
		t.Fatalf("sessionOwnerKey(no identity) = %q, want empty", got)
	}
	alice, err := serverauth.GrantFromRule(serverauth.TokenRule{Token: "a", UserID: "usr_alice", Scopes: serverauth.AllClientScopes()})
	if err != nil {
		t.Fatalf("GrantFromRule(alice): %v", err)
	}
	bob, err := serverauth.GrantFromRule(serverauth.TokenRule{Token: "b", UserID: "usr_bob", Scopes: serverauth.AllClientScopes()})
	if err != nil {
		t.Fatalf("GrantFromRule(bob): %v", err)
	}
	if sessionOwnerKey(alice) == sessionOwnerKey(bob) {
		t.Fatal("distinct users produced identical owner keys")
	}
	if scopedWorkspaceKey("", "ws") != "ws" {
		t.Fatal("empty owner key must leave workspace ID unchanged")
	}
	if scopedWorkspaceKey(sessionOwnerKey(alice), "ws") == "ws" {
		t.Fatal("scoped owner key must namespace the workspace ID")
	}
}
