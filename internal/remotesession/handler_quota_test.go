package remotesession

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// go-server-1021-3 over HTTP: a refused save answers 429 when the account keeps
// too many sessions, 413 when the payload would exceed its bytes and 507 at the
// disk's free-space floor, with a message the client shows; it is audited, and
// another user's saves are unaffected.
func TestHandlerQuotaRefusalsAreClearAndPerOwner(t *testing.T) {
	store := NewStoreWithQuota(t.TempDir(), Quota{MaxSessionsPerOwner: 2, MaxBytesPerOwner: 64, MinFreeBytes: 1 << 20})
	free := uint64(1 << 30)
	store.freeSpace = func(string) (uint64, error) { return free, nil }
	audit := &auditRecorder{}
	alice := NewHandlerWithOptions(store, HandlerOptions{Authorizer: sessionAuthzForUser(t, "token-a", "usr_alice"), AuditLogger: audit})
	bob := NewHandlerWithAuthorizer(store, sessionAuthzForUser(t, "token-b", "usr_bob"))
	put := func(h http.Handler, token, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/v1/sessions/"+id, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	expect := func(rec *httptest.ResponseRecorder, status int, message string) {
		t.Helper()
		if rec.Code != status || !strings.Contains(rec.Body.String(), message) {
			t.Fatalf("status %d body %q, want %d with %q", rec.Code, rec.Body.String(), status, message)
		}
	}

	expect(put(alice, "token-a", "ws-1", `{}`), http.StatusNoContent, "")
	expect(put(alice, "token-a", "ws-2", `{}`), http.StatusNoContent, "")
	expect(put(alice, "token-a", "ws-3", `{}`), http.StatusTooManyRequests, "session quota exceeded: this account stores 2 sessions and may keep 2")
	if last := audit.events[len(audit.events)-1]; last.Status != http.StatusTooManyRequests || !strings.Contains(last.Error, "session quota exceeded") {
		t.Fatalf("audit event = %+v", last)
	}
	expect(put(alice, "token-a", "ws-1", strings.Repeat("x", 100)), http.StatusRequestEntityTooLarge, "would bring this account's sessions to 102 bytes of the 64 bytes it may store")
	expect(put(bob, "token-b", "ws-1", `{}`), http.StatusNoContent, "")

	free = 1 << 20
	expect(put(bob, "token-b", "ws-2", `{}`), http.StatusInsufficientStorage, "session storage is full")
	// The client is not told the server's floor or free space; the audit log is.
	rec := put(alice, "token-a", "ws-1", `{}`)
	expect(rec, http.StatusInsufficientStorage, "session storage is full")
	if strings.Contains(rec.Body.String(), "MiB") {
		t.Fatalf("507 body reveals the server's disk: %q", rec.Body.String())
	}
	if last := audit.events[len(audit.events)-1]; last.Status != http.StatusInsufficientStorage || !strings.Contains(last.Error, "floor 1 MiB, 1 MiB free") {
		t.Fatalf("audit event = %+v, want the floor and the free space", last)
	}

	// The client reports the server's message.
	server := httptest.NewServer(alice)
	defer server.Close()
	free = 1 << 30
	err := NewClient(server.URL, "token-a").Save(context.Background(), "ws-3", []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "session quota exceeded") {
		t.Fatalf("client error = %v", err)
	}
}
