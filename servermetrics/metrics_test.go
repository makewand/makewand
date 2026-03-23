package servermetrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecorder_MiddlewareAndRenderPrometheus(t *testing.T) {
	recorder := NewRecorder()
	handler := recorder.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/sessions/repo-main":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/admin/users/usr_123/role":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/sessions/repo-main", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/admin/users/usr_123/role", nil))

	rendered := recorder.RenderPrometheus()
	if !strings.Contains(rendered, `path="/v1/sessions/:workspace"`) {
		t.Fatalf("metrics output missing normalized session path: %s", rendered)
	}
	if !strings.Contains(rendered, `path="/v1/admin/users/:id/role"`) {
		t.Fatalf("metrics output missing normalized user role path: %s", rendered)
	}
	if !strings.Contains(rendered, "makewand_http_requests_total") {
		t.Fatalf("metrics output missing request counter: %s", rendered)
	}
}

func TestRecorder_MiddlewarePreservesFlusher(t *testing.T) {
	recorder := NewRecorder()
	handler := recorder.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped response writer does not implement http.Flusher")
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if !rec.Flushed {
		t.Fatal("underlying recorder was not flushed")
	}
}
