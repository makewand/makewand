package servermetrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHistogramActiveAndFinalResponseStatus(t *testing.T) {
	r := NewRecorder()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	}()
	<-entered
	if !strings.Contains(r.RenderPrometheus(), "makewand_http_active_requests 1") {
		t.Fatal("active request missing")
	}
	close(release)
	<-done
	r.observe("GET", "/ready", 200, 100*time.Millisecond)
	r.ObserveError("usage", "")
	text := r.RenderPrometheus()
	for _, want := range []string{"makewand_http_active_requests 0", `path="/health",status="200"`, `path="/ready",status="200",le="0.1"} 1`, `makewand_errors_total{kind="usage",provider=""} 1`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s", want)
		}
	}
}
