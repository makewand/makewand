package router

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestReadinessDoesNotExposeFailureOrChangeLiveness(t *testing.T) {
	r := mustNewRouter(RouterConfig{})
	errorsObserved := 0
	h := r.HTTPHandler(HTTPHandlerOptions{ReadinessCheck: func(context.Context) error { return errors.New("private database path") }, Observer: func(kind, provider string) {
		if kind == "readiness" {
			errorsObserved++
		}
	}})
	ready := httptest.NewRecorder()
	h.ServeHTTP(ready, httptest.NewRequest("GET", "/ready", nil))
	if ready.Code != 503 || errorsObserved != 1 {
		t.Fatalf("readiness=%d observed=%d", ready.Code, errorsObserved)
	}
	health := httptest.NewRecorder()
	h.ServeHTTP(health, httptest.NewRequest("GET", "/health", nil))
	if health.Code != 200 {
		t.Fatal("liveness should remain available")
	}
}
