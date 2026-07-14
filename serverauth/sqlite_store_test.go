package serverauth

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSQLiteStore_IssueAuthenticateAndRevoke(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	view, tokenValue, err := store.Issue(TokenRule{
		ID:     "runner",
		Scopes: AllClientScopes(),
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if view.ID != "runner" {
		t.Fatalf("view.ID = %q, want %q", view.ID, "runner")
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tokenValue)
	if _, ok := store.AuthenticateRequest(req); !ok {
		t.Fatal("AuthenticateRequest(issued token) = false, want true")
	}

	if err := store.Revoke("runner"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := store.AuthenticateRequest(req); ok {
		t.Fatal("AuthenticateRequest(revoked token) = true, want false")
	}
}

func TestMultiAuthorizer_TriesInOrder(t *testing.T) {
	first := NewSingleTokenAuthorizer("alpha")
	second := NewSingleTokenAuthorizer("beta")
	authz := NewMultiAuthorizer(first, second)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer beta")
	if grant, ok := authz.AuthenticateRequest(req); !ok || grant == nil {
		t.Fatal("AuthenticateRequest(beta) = false, want true")
	}
}
