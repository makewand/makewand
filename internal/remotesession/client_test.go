package remotesession

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestClientDistinctEscapedWorkspaceIDs(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{
		{Token: "alice", UserID: "usr_alice", Scopes: serverauth.AllClientScopes()},
		{Token: "bob", UserID: "usr_bob", Scopes: serverauth.AllClientScopes()},
		{Token: "alice-read", UserID: "usr_alice", Scopes: []string{serverauth.ScopeSessionsRead}, WorkspacePrefixes: []string{"repo%"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/sessions/", NewHandlerWithAuthorizer(NewStore(t.TempDir()), authz))
	server := httptest.NewServer(mux)
	defer server.Close()
	alice := NewClient(server.URL, "alice")
	bob := NewClient(server.URL, "bob")
	readOnly := NewClient(server.URL, "alice-read")
	ids := []string{"repo/main", "repo%2Fmain", "repo%252Fmain", "repo%main", "repo/主目录", "repo%2F主目录", "repo space/#main?"}
	ctx := context.Background()
	payload := func(owner, id string) []byte {
		return []byte(fmt.Sprintf(`{"owner":%q,"id":%q}`, owner, id))
	}
	assertLoad := func(client *Client, owner, id string) {
		t.Helper()
		got, err := client.Load(ctx, id)
		if err != nil || string(got) != string(payload(owner, id)) {
			t.Fatalf("%s Load(%q) = %s, %v", owner, id, got, err)
		}
	}
	for _, id := range ids {
		if err := alice.Save(ctx, id, payload("alice", id)); err != nil {
			t.Fatalf("alice Save(%q): %v", id, err)
		}
		if _, err := bob.Load(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("bob Load new workspace %q: %v, want ErrNotFound", id, err)
		}
		if err := bob.Save(ctx, id, payload("bob", id)); err != nil {
			t.Fatalf("bob Save(%q): %v", id, err)
		}
	}
	for _, id := range ids {
		assertLoad(alice, "alice", id)
		assertLoad(bob, "bob", id)
	}
	// Workspace restrictions apply to the original ID, including its literal
	// percent sign, and a read-only grant still cannot mutate the session.
	assertLoad(readOnly, "alice", "repo%2Fmain")
	if _, err := readOnly.Load(ctx, "repo/main"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("read-only Load outside prefix: %v", err)
	}
	if err := readOnly.Save(ctx, "repo%2Fmain", []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("read-only Save: %v", err)
	}
	if err := readOnly.Delete(ctx, "repo%2Fmain"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("read-only Delete: %v", err)
	}
	for i, id := range ids {
		if err := alice.Delete(ctx, id); err != nil {
			t.Fatalf("alice Delete(%q): %v", id, err)
		}
		if _, err := alice.Load(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("alice Load deleted %q: %v", id, err)
		}
		for _, remaining := range ids[i+1:] {
			assertLoad(alice, "alice", remaining)
		}
		assertLoad(bob, "bob", id)
	}
}
