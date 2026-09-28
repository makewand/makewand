package servermetrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type labelKey struct {
	Method string
	Path   string
	Status int
}

type Recorder struct {
	mu         sync.Mutex
	counts     map[labelKey]int64
	durationMS map[labelKey]int64
}

func NewRecorder() *Recorder {
	return &Recorder{
		counts:     make(map[labelKey]int64),
		durationMS: make(map[labelKey]int64),
	}
}

func (r *Recorder) Middleware(next http.Handler) http.Handler {
	if r == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapStatusRecorder(rec), req)
		r.observe(req.Method, req.URL.Path, rec.status, time.Since(start))
	})
}

func (r *Recorder) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(r.RenderPrometheus()))
	})
}

func (r *Recorder) RenderPrometheus() string {
	if r == nil {
		return ""
	}
	// Snapshot under the lock and format outside it so a scrape never blocks
	// request accounting for longer than a map copy.
	r.mu.Lock()
	keys := make([]labelKey, 0, len(r.counts))
	counts := make(map[labelKey]int64, len(r.counts))
	durations := make(map[labelKey]int64, len(r.counts))
	for key, count := range r.counts {
		keys = append(keys, key)
		counts[key] = count
		durations[key] = r.durationMS[key]
	}
	r.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Path != keys[j].Path {
			return keys[i].Path < keys[j].Path
		}
		if keys[i].Method != keys[j].Method {
			return keys[i].Method < keys[j].Method
		}
		return keys[i].Status < keys[j].Status
	})

	var b strings.Builder
	b.WriteString("# HELP makewand_http_requests_total Total HTTP requests handled by makewand serve.\n")
	b.WriteString("# TYPE makewand_http_requests_total counter\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "makewand_http_requests_total{method=%q,path=%q,status=%q} %d\n",
			key.Method, key.Path, statusLabel(key.Status), counts[key])
	}
	b.WriteString("# HELP makewand_http_request_duration_ms_sum Sum of HTTP request durations in milliseconds.\n")
	b.WriteString("# TYPE makewand_http_request_duration_ms_sum counter\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "makewand_http_request_duration_ms_sum{method=%q,path=%q,status=%q} %d\n",
			key.Method, key.Path, statusLabel(key.Status), durations[key])
	}
	return b.String()
}

// maxSeries bounds the number of label combinations the recorder keeps. Route
// templates, the method allowlist, and status codes already bound cardinality;
// this is a defensive ceiling. Observations beyond it are folded into a single
// overflow series.
const maxSeries = 512

var overflowKey = labelKey{Method: otherMethod, Path: otherPath, Status: 0}

func statusLabel(status int) string {
	if status <= 0 {
		return "other"
	}
	return fmt.Sprintf("%d", status)
}

func (r *Recorder) observe(method, path string, status int, duration time.Duration) {
	if r == nil {
		return
	}
	key := labelKey{
		Method: normalizeMethod(method),
		Path:   normalizePath(path),
		Status: status,
	}
	if key.Status < 100 || key.Status > 999 {
		key.Status = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.counts[key]; !ok && len(r.counts) >= maxSeries {
		key = overflowKey
	}
	r.counts[key]++
	r.durationMS[key] += duration.Milliseconds()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func wrapStatusRecorder(rec *statusRecorder) http.ResponseWriter {
	if _, ok := rec.ResponseWriter.(http.Flusher); ok {
		return &flushStatusRecorder{statusRecorder: rec}
	}
	return rec
}

type flushStatusRecorder struct {
	*statusRecorder
}

func (r *flushStatusRecorder) Flush() {
	r.ResponseWriter.(http.Flusher).Flush()
}

const (
	otherPath   = "other"
	otherMethod = "OTHER"
)

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

func normalizeMethod(method string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if knownMethods[method] {
		return method
	}
	return otherMethod
}

// exactRoutes are the fixed paths served by `makewand serve`.
var exactRoutes = map[string]bool{
	"/health":                            true,
	"/metrics":                           true,
	"/admin":                             true,
	"/v1/chat/completions":               true,
	"/v1/responses":                      true,
	"/v1/models":                         true,
	"/v1/users/login":                    true,
	"/v1/users/register":                 true,
	"/v1/admin/session/login":            true,
	"/v1/admin/session/logout":           true,
	"/v1/admin/session/me":               true,
	"/v1/admin/tokens":                   true,
	"/v1/admin/audit/summary":            true,
	"/v1/admin/audit/events":             true,
	"/v1/admin/dashboard":                true,
	"/v1/admin/billing/summary":          true,
	"/v1/admin/billing/periods":          true,
	"/v1/admin/billing/alerts":           true,
	"/v1/admin/usage/summary":            true,
	"/v1/admin/usage/events":             true,
	"/v1/admin/users":                    true,
	"/v1/admin/organizations":            true,
	"/v1/admin/projects":                 true,
	"/v1/admin/organization-memberships": true,
	"/v1/admin/project-memberships":      true,
}

var userActions = map[string]bool{"activate": true, "deactivate": true, "role": true, "password": true}

// normalizePath maps a request path to its route template. Anything that is
// not a known route — including unauthenticated 404 probes — becomes "other",
// so clients cannot create new label values.
func normalizePath(path string) string {
	if exactRoutes[path] {
		return path
	}
	switch {
	case strings.HasPrefix(path, "/v1/sessions/"):
		return "/v1/sessions/:workspace"
	case strings.HasPrefix(path, "/admin/"):
		return "/admin/*"
	case strings.HasPrefix(path, "/v1/admin/tokens/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/admin/tokens/"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] == "revoke" {
			return "/v1/admin/tokens/:id/revoke"
		}
	case strings.HasPrefix(path, "/v1/admin/users/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/admin/users/"), "/")
		if len(parts) == 2 && parts[0] != "" && userActions[parts[1]] {
			return "/v1/admin/users/:id/" + parts[1]
		}
	}
	return otherPath
}
