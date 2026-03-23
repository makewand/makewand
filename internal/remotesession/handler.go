package remotesession

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
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

		switch req.Method {
		case http.MethodGet:
			data, err := store.Load(workspaceID)
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
			_, _ = w.Write(data)
			event.Status = http.StatusOK
		case http.MethodPut:
			data, err := io.ReadAll(io.LimitReader(req.Body, maxSessionPayloadBytes))
			if err != nil {
				event.Status = http.StatusBadRequest
				event.Error = err.Error()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := store.Save(workspaceID, data); err != nil {
				event.Status = http.StatusInternalServerError
				event.Error = err.Error()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			event.Status = http.StatusNoContent
		case http.MethodDelete:
			if err := store.Delete(workspaceID); err != nil {
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
