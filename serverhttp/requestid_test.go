package serverhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithRequestID_PreservesInboundHeader(t *testing.T) {
	handler := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := RequestIDFromRequest(req); got != "req_custom" {
			t.Fatalf("RequestIDFromRequest() = %q, want %q", got, "req_custom")
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(HeaderRequestID, "req_custom")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderRequestID); got != "req_custom" {
		t.Fatalf("response request ID = %q, want %q", got, "req_custom")
	}
}

func TestWithRequestID_GeneratesWhenMissing(t *testing.T) {
	handler := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := RequestIDFromRequest(req); got == "" {
			t.Fatal("RequestIDFromRequest() = empty, want generated ID")
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if got := rec.Header().Get(HeaderRequestID); got == "" {
		t.Fatal("response request ID = empty, want generated ID")
	}
}
