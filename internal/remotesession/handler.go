package remotesession

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxSessionPayloadBytes = 4 << 20 // 4 MiB

// NewHandler returns an HTTP handler for remote session CRUD.
func NewHandler(store *Store, bearerToken string) http.Handler {
	return withAuth(strings.TrimSpace(bearerToken), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if store == nil {
			http.Error(w, "session store unavailable", http.StatusServiceUnavailable)
			return
		}
		workspaceID, ok := sessionIDFromPath(req.URL.Path)
		if !ok {
			http.NotFound(w, req)
			return
		}

		switch req.Method {
		case http.MethodGet:
			data, err := store.Load(workspaceID)
			if err != nil {
				if err == ErrNotFound {
					http.NotFound(w, req)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		case http.MethodPut:
			data, err := io.ReadAll(io.LimitReader(req.Body, maxSessionPayloadBytes))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := store.Save(workspaceID, data); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			if err := store.Delete(workspaceID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
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
