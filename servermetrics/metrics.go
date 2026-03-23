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
		r.observe(req.Method, normalizePath(req.URL.Path), rec.status, time.Since(start))
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
	r.mu.Lock()
	defer r.mu.Unlock()

	keys := make([]labelKey, 0, len(r.counts))
	for key := range r.counts {
		keys = append(keys, key)
	}
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
		b.WriteString(fmt.Sprintf("makewand_http_requests_total{method=%q,path=%q,status=%q} %d\n",
			key.Method, key.Path, fmt.Sprintf("%d", key.Status), r.counts[key]))
	}
	b.WriteString("# HELP makewand_http_request_duration_ms_sum Sum of HTTP request durations in milliseconds.\n")
	b.WriteString("# TYPE makewand_http_request_duration_ms_sum counter\n")
	for _, key := range keys {
		b.WriteString(fmt.Sprintf("makewand_http_request_duration_ms_sum{method=%q,path=%q,status=%q} %d\n",
			key.Method, key.Path, fmt.Sprintf("%d", key.Status), r.durationMS[key]))
	}
	return b.String()
}

func (r *Recorder) observe(method, path string, status int, duration time.Duration) {
	if r == nil {
		return
	}
	key := labelKey{
		Method: strings.ToUpper(strings.TrimSpace(method)),
		Path:   normalizePath(path),
		Status: status,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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

func normalizePath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/sessions/"):
		return "/v1/sessions/:workspace"
	case strings.HasPrefix(path, "/v1/admin/tokens/") && strings.HasSuffix(path, "/revoke"):
		return "/v1/admin/tokens/:id/revoke"
	case strings.HasPrefix(path, "/v1/admin/users/") && strings.HasSuffix(path, "/activate"):
		return "/v1/admin/users/:id/activate"
	case strings.HasPrefix(path, "/v1/admin/users/") && strings.HasSuffix(path, "/deactivate"):
		return "/v1/admin/users/:id/deactivate"
	case strings.HasPrefix(path, "/v1/admin/users/") && strings.HasSuffix(path, "/role"):
		return "/v1/admin/users/:id/role"
	default:
		return path
	}
}
