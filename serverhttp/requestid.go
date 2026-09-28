package serverhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

const HeaderRequestID = "X-Request-Id"

// MaxRequestIDLength bounds client-supplied request IDs. Longer values are
// replaced by a generated ID.
const MaxRequestIDLength = 128

// ValidRequestID reports whether a client-supplied request ID may be echoed
// into response headers, audit records, and usage records: 1 to
// MaxRequestIDLength bytes of ASCII letters, digits, and "-_.:/".
func ValidRequestID(value string) bool {
	if value == "" || len(value) > MaxRequestIDLength {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == ':' || c == '/':
		default:
			return false
		}
	}
	return true
}

type requestIDContextKey struct{}

// WithRequestID ensures every request has a stable request ID available in
// context and echoed back in the response header. A client-supplied ID is kept
// only when ValidRequestID accepts it; otherwise a new ID is generated.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestID := strings.TrimSpace(req.Header.Get(HeaderRequestID))
		if !ValidRequestID(requestID) {
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
	if value := strings.TrimSpace(req.Header.Get(HeaderRequestID)); ValidRequestID(value) {
		return value
	}
	return ""
}

func generateRequestID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "req_fallback"
	}
	return "req_" + hex.EncodeToString(buf)
}
