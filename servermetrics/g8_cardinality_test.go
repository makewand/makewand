package servermetrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seriesCount(rendered string) int {
	count := 0
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "makewand_http_requests_total{") {
			count++
		}
	}
	return count
}

// go-server#6: unauthenticated requests to arbitrary paths/methods must not
// create new label values; unknown paths collapse into "other".
func TestG8_MetricsLabelsAreBoundedRouteTemplates(t *testing.T) {
	recorder := NewRecorder()
	handler := recorder.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.NotFound(w, req)
	}))
	for i := 0; i < 300; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, fmt.Sprintf("/probe/%d", i), nil))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(fmt.Sprintf("X%d", i), "/v1/models", nil))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/admin/users/u%d/unknown", i), nil))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/asset-%d.js", i), nil))
	}
	rendered := recorder.RenderPrometheus()
	if strings.Contains(rendered, "/probe/") || strings.Contains(rendered, "/u1/") || strings.Contains(rendered, "asset-") {
		t.Fatalf("raw request paths leaked into labels:\n%s", rendered)
	}
	if strings.Contains(rendered, `method="X1"`) {
		t.Fatalf("arbitrary methods leaked into labels:\n%s", rendered)
	}
	if !strings.Contains(rendered, `path="other"`) || !strings.Contains(rendered, `method="OTHER"`) || !strings.Contains(rendered, `path="/admin/*"`) {
		t.Fatalf("expected other/OTHER buckets:\n%s", rendered)
	}
	if got := seriesCount(rendered); got > 8 {
		t.Fatalf("%d series after 1200 junk requests, want a handful:\n%s", got, rendered)
	}
}

func TestG8_MetricsKnownRoutesUseTemplates(t *testing.T) {
	cases := map[string]string{
		"/health":                            "/health",
		"/metrics":                           "/metrics",
		"/v1/chat/completions":               "/v1/chat/completions",
		"/v1/responses":                      "/v1/responses",
		"/v1/models":                         "/v1/models",
		"/v1/users/login":                    "/v1/users/login",
		"/v1/users/register":                 "/v1/users/register",
		"/v1/sessions/repo-main":             "/v1/sessions/:workspace",
		"/v1/admin/tokens":                   "/v1/admin/tokens",
		"/v1/admin/tokens/tok_1/revoke":      "/v1/admin/tokens/:id/revoke",
		"/v1/admin/users":                    "/v1/admin/users",
		"/v1/admin/users/usr_1/password":     "/v1/admin/users/:id/password",
		"/v1/admin/users/usr_1/role":         "/v1/admin/users/:id/role",
		"/v1/admin/organization-memberships": "/v1/admin/organization-memberships",
		"/v1/admin/session/login":            "/v1/admin/session/login",
		"/v1/admin/billing/summary":          "/v1/admin/billing/summary",
		"/admin":                             "/admin",
		"/admin/app.js":                      "/admin/*",
		"/v1/admin/tokens/a/b/revoke":        "other",
		"/v1/admin/users/usr_1/role/extra":   "other",
		"/v1/admin/does-not-exist":           "other",
		"/random":                            "other",
	}
	for path, want := range cases {
		if got := normalizePath(path); got != want {
			t.Errorf("normalizePath(%q)=%q, want %q", path, got, want)
		}
	}
}

// A hard cap protects the recorder even if a future route template explodes.
func TestG8_MetricsSeriesCapFoldsOverflow(t *testing.T) {
	recorder := NewRecorder()
	for status := 100; status < 100+maxSeries+50; status++ {
		recorder.observe(http.MethodGet, "/health", status, 0)
	}
	recorder.mu.Lock()
	n := len(recorder.counts)
	recorder.mu.Unlock()
	if n > maxSeries+1 {
		t.Fatalf("recorder holds %d series, cap is %d", n, maxSeries)
	}
	if !strings.Contains(recorder.RenderPrometheus(), `path="other",status="other"`) {
		t.Fatal("overflow series not rendered")
	}
}
