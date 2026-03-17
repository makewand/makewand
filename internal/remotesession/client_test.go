package remotesession

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
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
