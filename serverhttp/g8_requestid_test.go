package serverhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// go-server#11: client-supplied X-Request-Id values are echoed into response
// headers and audit/usage records, so they must be bounded and restricted to a
// safe character set; anything else is replaced by a generated ID.
func TestG8_RequestIDIsBoundedAndFiltered(t *testing.T) {
	cases := map[string]bool{
		"req_custom":                              true,
		"4f7c2a10-8e2b-4a55-9f0e-2c9f3c8e1a77":    true,
		"trace.id:span-1/2":                       true,
		strings.Repeat("A", 8000):                 false,
		strings.Repeat("a", MaxRequestIDLength+1): false,
		"has space":                               false,
		"quote\"inject":                           false,
		"new\\nline":                              false,
		"<script>":                                false,
		"日本語":                                     false,
	}
	for value, keep := range cases {
		var seen string
		handler := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			seen = RequestIDFromRequest(req)
		}))
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header[HeaderRequestID] = []string{value}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		echoed := rec.Header().Get(HeaderRequestID)
		if echoed != seen {
			t.Fatalf("context %q != header %q", seen, echoed)
		}
		if keep && echoed != value {
			t.Fatalf("valid request ID %q replaced by %q", value, echoed)
		}
		if !keep && (echoed == value || !strings.HasPrefix(echoed, "req_")) {
			t.Fatalf("unsafe request ID %.40q kept (echoed %.40q)", value, echoed)
		}
		if len(echoed) > MaxRequestIDLength {
			t.Fatalf("echoed request ID has %d bytes", len(echoed))
		}
	}

	// Without the middleware the raw header fallback is filtered the same way.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(HeaderRequestID, strings.Repeat("B", 8000))
	if got := RequestIDFromRequest(req); got != "" {
		t.Fatalf("unfiltered fallback request ID of %d bytes", len(got))
	}
}
