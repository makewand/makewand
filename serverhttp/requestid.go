package serverhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

const HeaderRequestID = "X-Request-Id"

type requestIDContextKey struct{}

// WithRequestID ensures every request has a stable request ID available in
// context and echoed back in the response header.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestID := strings.TrimSpace(req.Header.Get(HeaderRequestID))
		if requestID == "" {
			requestID = generateRequestID()
		}
		w.Header().Set(HeaderRequestID, requestID)
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), requestIDContextKey{}, requestID)))
	})
}

// RequestIDFromContext extracts the request ID attached by WithRequestID.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return strings.TrimSpace(value)
}

// RequestIDFromRequest returns the request ID from context, falling back to
// the inbound header when middleware has not attached context yet.
func RequestIDFromRequest(req *http.Request) string {
	if req == nil {
		return ""
	}
	if value := RequestIDFromContext(req.Context()); value != "" {
		return value
	}
	return strings.TrimSpace(req.Header.Get(HeaderRequestID))
}

func generateRequestID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "req_fallback"
	}
	return "req_" + hex.EncodeToString(buf)
}
