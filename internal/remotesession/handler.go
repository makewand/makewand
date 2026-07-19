package remotesession

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverhttp"
)

const maxSessionPayloadBytes = 4 << 20 // 4 MiB

// HandlerOptions configures the session HTTP handler.
type HandlerOptions struct {
	Authorizer  serverauth.RequestAuthorizer
	AuditLogger serveraudit.Logger
}

// NewHandler returns an HTTP handler for remote session CRUD.
func NewHandler(store *Store, bearerToken string) http.Handler {
	return NewHandlerWithOptions(store, HandlerOptions{
		Authorizer: serverauth.NewSingleTokenAuthorizer(strings.TrimSpace(bearerToken)),
	})
}

// NewHandlerWithAuthorizer returns an HTTP handler for remote session CRUD with
// scoped token enforcement.
func NewHandlerWithAuthorizer(store *Store, authz serverauth.RequestAuthorizer) http.Handler {
	return NewHandlerWithOptions(store, HandlerOptions{Authorizer: authz})
}

// NewHandlerWithOptions returns an HTTP handler for remote session CRUD with
// scoped token enforcement and optional audit logging.
func NewHandlerWithOptions(store *Store, opts HandlerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		event := serveraudit.Event{
			Timestamp: time.Now().UTC(),
			RequestID: serverhttp.RequestIDFromRequest(req),
			Kind:      "session",
			Method:    req.Method,
			Path:      req.URL.Path,
		}
		defer func() {
			if opts.AuditLogger == nil {
				return
			}
			if event.DurationMS == 0 {
				event.DurationMS = time.Since(start).Milliseconds()
			}
			opts.AuditLogger.Log(event)
		}()

		if store == nil {
			event.Status = http.StatusServiceUnavailable
			event.Error = "session store unavailable"
			http.Error(w, "session store unavailable", http.StatusServiceUnavailable)
			return
		}
		var (
			grant *serverauth.Grant
			ok    bool
		)
		if opts.Authorizer == nil {
			ok = true
		} else {
			grant, ok = opts.Authorizer.AuthenticateRequest(req)
		}
		if !ok {
			event.Status = http.StatusUnauthorized
			event.Error = "unauthorized"
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if grant != nil {
			event.TokenID = grant.TokenID()
			event.TokenDescription = grant.Description()
		}
		if err := grant.CheckAndConsumeRequestAt(time.Now()); err != nil {
			event.Status = http.StatusTooManyRequests
			event.Error = err.Error()
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		workspaceID, ok := sessionIDFromPath(req.URL.Path)
		if !ok {
			event.Status = http.StatusNotFound
			event.Error = "session not found"
			http.NotFound(w, req)
			return
		}
		event.WorkspaceID = workspaceID
		if !grant.AllowsWorkspace(workspaceID) {
			event.Status = http.StatusForbidden
			event.Error = "forbidden"
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		scope, methodAllowed := sessionScopeForMethod(req.Method)
		event.Scope = scope
		if !methodAllowed {
			event.Status = http.StatusMethodNotAllowed
			event.Error = "method not allowed"
			w.Header().Set("Allow", "GET, PUT, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !grant.AllowsScope(scope) {
			event.Status = http.StatusForbidden
			event.Error = "forbidden"
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// Namespace the stored key by the caller's tenant identity so a session
		// created by one owner is never reachable by another, even if they guess
		// the workspace ID. When the grant carries no identity (single-user or
		// no-auth mode, and legacy operator-issued full-access tokens), the key
		// is unchanged and existing sessions continue to resolve.
		storageKey := scopedWorkspaceKey(sessionOwnerKey(grant), workspaceID)

		switch req.Method {
		case http.MethodGet:
			data, err := store.Load(storageKey)
			if err != nil {
				if err == ErrNotFound {
					event.Status = http.StatusNotFound
					event.Error = ErrNotFound.Error()
					http.NotFound(w, req)
					return
				}
				event.Status = http.StatusInternalServerError
				event.Error = err.Error()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			//nolint:gosec // G705: JSON session payload response (Content-Type application/json), not an HTML sink.
			_, _ = w.Write(data)
			event.Status = http.StatusOK
		case http.MethodPut:
			data, err := io.ReadAll(io.LimitReader(req.Body, maxSessionPayloadBytes+1))
			if err != nil {
				event.Status = http.StatusBadRequest
				event.Error = err.Error()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if len(data) > maxSessionPayloadBytes {
				event.Status = http.StatusRequestEntityTooLarge
				event.Error = "session payload too large"
				http.Error(w, "session payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			if err := store.Save(storageKey, data); err != nil {
				event.Status = http.StatusInternalServerError
				event.Error = err.Error()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			event.Status = http.StatusNoContent
		case http.MethodDelete:
			if err := store.Delete(storageKey); err != nil {
				event.Status = http.StatusInternalServerError
				event.Error = err.Error()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			event.Status = http.StatusNoContent
		default:
			event.Status = http.StatusMethodNotAllowed
			event.Error = "method not allowed"
			w.Header().Set("Allow", "GET, PUT, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// sessionOwnerKey derives a stable per-tenant namespace from the caller's
// grant. It returns an empty string when the grant carries no identity, which
// preserves single-user/no-auth behavior and legacy operator tokens: their
// keys stay equal to the bare workspace ID. Every field is length-prefixed so
// distinct identities can never collide into the same namespace.
func sessionOwnerKey(grant *serverauth.Grant) string {
	if grant == nil {
		return ""
	}
	userID := strings.TrimSpace(grant.UserID())
	orgID := strings.TrimSpace(grant.OrganizationID())
	projectID := strings.TrimSpace(grant.ProjectID())
	if userID == "" && orgID == "" && projectID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("org:")
	b.WriteString(strconv.Itoa(len(orgID)))
	b.WriteByte(':')
	b.WriteString(orgID)
	b.WriteString("|proj:")
	b.WriteString(strconv.Itoa(len(projectID)))
	b.WriteByte(':')
	b.WriteString(projectID)
	b.WriteString("|user:")
	b.WriteString(strconv.Itoa(len(userID)))
	b.WriteByte(':')
	b.WriteString(userID)
	return b.String()
}

// scopedWorkspaceKey combines the owner namespace with the workspace ID. When
// ownerKey is empty the workspace ID is returned unchanged for backward
// compatibility with pre-tenancy stores.
func scopedWorkspaceKey(ownerKey, workspaceID string) string {
	if ownerKey == "" {
		return workspaceID
	}
	return ownerKey + "\x1e" + workspaceID
}

func sessionScopeForMethod(method string) (string, bool) {
	switch method {
	case http.MethodGet:
		return serverauth.ScopeSessionsRead, true
	case http.MethodPut:
		return serverauth.ScopeSessionsWrite, true
	case http.MethodDelete:
		return serverauth.ScopeSessionsDelete, true
	default:
		return "", false
	}
}

func sessionIDFromPath(path string) (string, bool) {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	raw := strings.TrimPrefix(path, prefix)
	if raw == "" {
		return "", false
	}
	workspaceID, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(workspaceID) == "" {
		return "", false
	}
	return workspaceID, true
}
