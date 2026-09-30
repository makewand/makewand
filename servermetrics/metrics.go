package servermetrics

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	seconds    map[labelKey]float64
	buckets    map[labelKey][]int64
	errors     map[errorKey]int64
	active     atomic.Int64
	dbStats    func() sql.DBStats
}

func NewRecorder() *Recorder {
	return &Recorder{
		counts:     make(map[labelKey]int64),
		durationMS: make(map[labelKey]int64),
		seconds:    make(map[labelKey]float64),
		buckets:    make(map[labelKey][]int64),
		errors:     make(map[errorKey]int64),
	}
}

func (r *Recorder) Middleware(next http.Handler) http.Handler {
	if r == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		r.active.Add(1)
		defer func() {
			r.active.Add(-1)
			if value := recover(); value != nil {
				rec.status = http.StatusInternalServerError
				r.observe(req.Method, req.URL.Path, rec.status, time.Since(start))
				panic(value)
			}
			r.observe(req.Method, req.URL.Path, rec.status, time.Since(start))
		}()
		next.ServeHTTP(wrapStatusRecorder(rec), req)
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
	seconds := make(map[labelKey]float64, len(r.counts))
	buckets := make(map[labelKey][]int64, len(r.counts))
	errors := make(map[errorKey]int64, len(r.errors))
	dbStats := r.dbStats
	for key, count := range r.errors {
		errors[key] = count
	}
	for key, count := range r.counts {
		keys = append(keys, key)
		counts[key] = count
		durations[key] = r.durationMS[key]
		seconds[key] = r.seconds[key]
		buckets[key] = append([]int64(nil), r.buckets[key]...)
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
	b.WriteString("# HELP makewand_http_active_requests Requests currently being handled.\n# TYPE makewand_http_active_requests gauge\n")
	fmt.Fprintf(&b, "makewand_http_active_requests %d\n", r.active.Load())
	b.WriteString("# HELP makewand_http_request_duration_seconds HTTP request latency.\n# TYPE makewand_http_request_duration_seconds histogram\n")
	for _, key := range keys {
		for i, bound := range latencyBuckets {
			fmt.Fprintf(&b, "makewand_http_request_duration_seconds_bucket{method=%q,path=%q,status=%q,le=%q} %d\n", key.Method, key.Path, statusLabel(key.Status), fmt.Sprintf("%g", bound), buckets[key][i])
		}
		fmt.Fprintf(&b, "makewand_http_request_duration_seconds_bucket{method=%q,path=%q,status=%q,le=%q} %d\n", key.Method, key.Path, statusLabel(key.Status), "+Inf", counts[key])
		fmt.Fprintf(&b, "makewand_http_request_duration_seconds_sum{method=%q,path=%q,status=%q} %g\n", key.Method, key.Path, statusLabel(key.Status), seconds[key])
		fmt.Fprintf(&b, "makewand_http_request_duration_seconds_count{method=%q,path=%q,status=%q} %d\n", key.Method, key.Path, statusLabel(key.Status), counts[key])
	}
	b.WriteString("# HELP makewand_errors_total Provider and infrastructure failures.\n# TYPE makewand_errors_total counter\n")
	errorKeys := make([]errorKey, 0, len(errors))
	for key := range errors {
		errorKeys = append(errorKeys, key)
	}
	sort.Slice(errorKeys, func(i, j int) bool {
		if errorKeys[i].Kind != errorKeys[j].Kind {
			return errorKeys[i].Kind < errorKeys[j].Kind
		}
		return errorKeys[i].Provider < errorKeys[j].Provider
	})
	for _, key := range errorKeys {
		fmt.Fprintf(&b, "makewand_errors_total{kind=%q,provider=%q} %d\n", key.Kind, key.Provider, errors[key])
	}
	if dbStats != nil {
		stats := dbStats()
		b.WriteString("# TYPE makewand_db_connections gauge\n# TYPE makewand_db_waits_total counter\n# TYPE makewand_db_wait_duration_seconds_total counter\n")
		fmt.Fprintf(&b, "makewand_db_connections{state=\"open\"} %d\nmakewand_db_connections{state=\"in_use\"} %d\nmakewand_db_connections{state=\"idle\"} %d\nmakewand_db_waits_total %d\nmakewand_db_wait_duration_seconds_total %g\n", stats.OpenConnections, stats.InUse, stats.Idle, stats.WaitCount, stats.WaitDuration.Seconds())
	}
	return b.String()
}

// SetDBStats exposes connection pressure for the usage ledger's pool.
func (r *Recorder) SetDBStats(fn func() sql.DBStats) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dbStats = fn
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
	r.seconds[key] += duration.Seconds()
	if r.buckets[key] == nil {
		r.buckets[key] = make([]int64, len(latencyBuckets))
	}
	for i, bound := range latencyBuckets {
		if duration.Seconds() <= bound {
			r.buckets[key][i]++
		}
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		r.ResponseWriter.WriteHeader(status)
		return
	}
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(data)
}
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
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
	"/ready":                             true,
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

var latencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300, 600}

type errorKey struct{ Kind, Provider string }

// ObserveError keeps both values bounded; no request ID or free-form error text
// is accepted as a label.
func (r *Recorder) ObserveError(kind, provider string) {
	if r == nil {
		return
	}
	switch kind {
	case "provider", "db", "usage", "webhook", "readiness":
	default:
		kind = "other"
	}
	provider = strings.TrimSpace(provider)
	if len(provider) > 64 {
		provider = "other"
	}
	key := errorKey{Kind: kind, Provider: provider}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.errors[key]; !ok && len(r.errors) >= 128 {
		key = errorKey{Kind: "other", Provider: "other"}
	}
	r.errors[key]++
}
