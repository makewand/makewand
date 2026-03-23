package remotesession

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
)

type auditRecorder struct {
	events []serveraudit.Event
}

func (r *auditRecorder) Log(event serveraudit.Event) {
	r.events = append(r.events, event)
}

func TestHandlerWithOptions_AuditLogsSuccessfulWrite(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:                "runner",
				Token:             "secret",
				Scopes:            []string{serverauth.ScopeSessionsWrite},
				WorkspacePrefixes: []string{"repo-"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	recorder := &auditRecorder{}
	store := NewStore(t.TempDir())
	handler := NewHandlerWithOptions(store, HandlerOptions{
		Authorizer:  authz,
		AuditLogger: recorder,
	})

	req := httptest.NewRequest(http.MethodPut, "/v1/sessions/repo-main", bytes.NewBufferString(`{"version":1}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Request-Id", "req_session")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Kind != "session" {
		t.Fatalf("Kind = %q, want %q", event.Kind, "session")
	}
	if event.TokenID != "runner" {
		t.Fatalf("TokenID = %q, want %q", event.TokenID, "runner")
	}
	if event.Scope != serverauth.ScopeSessionsWrite {
		t.Fatalf("Scope = %q, want %q", event.Scope, serverauth.ScopeSessionsWrite)
	}
	if event.WorkspaceID != "repo-main" {
		t.Fatalf("WorkspaceID = %q, want %q", event.WorkspaceID, "repo-main")
	}
	if event.Status != http.StatusNoContent {
		t.Fatalf("Status = %d, want 204", event.Status)
	}
	if event.RequestID != "req_session" {
		t.Fatalf("RequestID = %q, want %q", event.RequestID, "req_session")
	}
}

func TestHandlerWithOptions_AuditLogsForbiddenWorkspace(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				ID:                "runner",
				Token:             "secret",
				Scopes:            []string{serverauth.ScopeSessionsWrite},
				WorkspacePrefixes: []string{"repo-"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	recorder := &auditRecorder{}
	store := NewStore(t.TempDir())
	handler := NewHandlerWithOptions(store, HandlerOptions{
		Authorizer:  authz,
		AuditLogger: recorder,
	})

	req := httptest.NewRequest(http.MethodPut, "/v1/sessions/other-main", bytes.NewBufferString(`{"version":1}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.WorkspaceID != "other-main" {
		t.Fatalf("WorkspaceID = %q, want %q", event.WorkspaceID, "other-main")
	}
	if event.Status != http.StatusForbidden {
		t.Fatalf("Status = %d, want 403", event.Status)
	}
	if event.Error == "" {
		t.Fatal("Error = empty, want forbidden audit message")
	}
}

func TestHandlerWithOptions_RejectsRequestsOverHourlyQuota(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:              "secret",
				Scopes:             []string{serverauth.ScopeSessionsRead},
				MaxRequestsPerHour: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	store := NewStore(t.TempDir())
	if err := store.Save("repo-main", []byte(`{"version":1}`)); err != nil {
		t.Fatalf("store.Save: %v", err)
	}
	handler := NewHandlerWithOptions(store, HandlerOptions{Authorizer: authz})

	req1 := httptest.NewRequest(http.MethodGet, "/v1/sessions/repo-main", nil)
	req1.Header.Set("Authorization", "Bearer secret")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/sessions/repo-main", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", rec2.Code)
	}
}

func TestHandlerWithOptions_RejectsRequestsOverDailyQuota(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{
			{
				Token:             "secret",
				Scopes:            []string{serverauth.ScopeSessionsRead},
				MaxRequestsPerDay: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	store := NewStore(t.TempDir())
	if err := store.Save("repo-main", []byte(`{"version":1}`)); err != nil {
		t.Fatalf("store.Save: %v", err)
	}
	handler := NewHandlerWithOptions(store, HandlerOptions{Authorizer: authz})

	req1 := httptest.NewRequest(http.MethodGet, "/v1/sessions/repo-main", nil)
	req1.Header.Set("Authorization", "Bearer secret")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/sessions/repo-main", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "max_requests_per_day") {
		t.Fatalf("second body = %q, want daily quota error", rec2.Body.String())
	}
}
