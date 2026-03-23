package remotesession

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/makewand/makewand/serverauth"
)

func TestClientRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	server := httptest.NewServer(NewHandler(store, "secret"))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	data := []byte(`{"saved_at":"2026-03-17T00:00:00Z","messages":[{"role":"user","content":"hi"}]}`)

	if err := client.Save(context.Background(), "workspace-1", data); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := client.Load(context.Background(), "workspace-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Load = %s, want %s", got, data)
	}

	if err := client.Delete(context.Background(), "workspace-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := client.Load(context.Background(), "workspace-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after delete error = %v, want ErrNotFound", err)
	}
}

func TestClientRoundTrip_ScopedWorkspacePrefix(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:             "secret",
				Scopes:            []string{serverauth.ScopeSessionsRead, serverauth.ScopeSessionsWrite, serverauth.ScopeSessionsDelete},
				WorkspacePrefixes: []string{"repo-"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	store := NewStore(t.TempDir())
	server := httptest.NewServer(NewHandlerWithAuthorizer(store, authz))
	defer server.Close()

	client := NewClient(server.URL, "secret")
	data := []byte(`{"saved_at":"2026-03-17T00:00:00Z","messages":[{"role":"user","content":"hi"}]}`)

	if err := client.Save(context.Background(), "repo-1", data); err != nil {
		t.Fatalf("Save(repo-1): %v", err)
	}

	if err := client.Save(context.Background(), "other-1", data); err == nil {
		t.Fatal("Save(other-1) error = nil, want forbidden error")
	}
}
