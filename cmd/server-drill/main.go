// server-drill executes repeatable loopback HTTP, SQLite WAL, crash, and recovery
// exercises with a deterministic local provider. It never contacts a model.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/makewand/makewand/internal/backup"
	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverdb"
	"github.com/makewand/makewand/serverhttp"
	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

type options struct {
	Output                                      string
	Requests, Concurrency, Tenants, RouterCalls int
	Timeout                                     time.Duration
	Child, ChildRestore                         bool
	StateDir, ReadyFile, Archive                string
	BudgetCalls                                 int
}
type check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}
type latency struct {
	ErrorSamples     []string `json:"error_samples,omitempty"`
	ProviderPeak     int64    `json:"provider_peak_concurrency"`
	Requests         int      `json:"requests"`
	Successful       int      `json:"successful"`
	ExpectedRejected int      `json:"expected_budget_rejections"`
	Unexpected       int      `json:"unexpected_errors"`
	P50MS            float64  `json:"p50_ms"`
	P95MS            float64  `json:"p95_ms"`
	RPS              float64  `json:"requests_per_second"`
	ErrorRate        float64  `json:"unexpected_error_rate"`
	DurationMS       int64    `json:"duration_ms"`
}
type recovery struct {
	BarrierRows          int64   `json:"barrier_rows"`
	RestoredRows         int64   `json:"restored_rows"`
	RPOAcknowledgedLost  int64   `json:"rpo_acknowledged_rows_lost"`
	RPOAfterSnapshotRows int64   `json:"rpo_post_snapshot_rows"`
	RPOAfterSnapshotMS   float64 `json:"rpo_post_snapshot_ms"`
	RTOMS                float64 `json:"rto_ms"`
	CrashRestartMS       float64 `json:"crash_restart_ms"`
	GracefulDrainMS      float64 `json:"graceful_drain_ms"`
	MigrationRows        int     `json:"migration_rows_preserved"`
}
type report struct {
	Schema      string         `json:"schema"`
	Started     time.Time      `json:"started_at"`
	Finished    time.Time      `json:"finished_at"`
	Status      string         `json:"status"`
	Environment map[string]any `json:"environment"`
	Profile     map[string]any `json:"profile"`
	Scope       string         `json:"scope"`
	HTTP        latency        `json:"http"`
	Recovery    recovery       `json:"recovery"`
	Checks      []check        `json:"checks"`
	Error       string         `json:"error,omitempty"`
}

func main() {
	o := options{}
	flag.StringVar(&o.Output, "output", "server-drill-report.json", "machine-readable report path")
	flag.IntVar(&o.Requests, "requests", 400, "loopback HTTP load requests")
	flag.IntVar(&o.Concurrency, "concurrency", 32, "concurrent HTTP workers")
	flag.IntVar(&o.Tenants, "tenants", 4, "independent token/org/project tenants")
	flag.IntVar(&o.RouterCalls, "router-calls", 100000, "calls for 50 independent Router identities; 0 disables")
	flag.DurationVar(&o.Timeout, "timeout", 5*time.Minute, "total exercise deadline")
	flag.BoolVar(&o.Child, "child", false, "internal local HTTP process")
	flag.BoolVar(&o.ChildRestore, "child-restore", false, "internal crash restore process")
	flag.StringVar(&o.StateDir, "state-dir", "", "internal temporary state")
	flag.StringVar(&o.ReadyFile, "ready-file", "", "internal ready marker")
	flag.StringVar(&o.Archive, "archive", "", "internal restore archive")
	flag.IntVar(&o.BudgetCalls, "budget-calls", 0, "internal tenant budget calls")
	flag.Parse()
	if o.Child || o.ChildRestore {
		var err error
		if o.Child {
			err = serveChild(o)
		} else {
			err = restoreChild(o)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if o.Requests < o.Tenants*2 || o.Concurrency < 1 || o.Tenants < 1 || o.Tenants > 50 || o.RouterCalls < 0 || o.Timeout <= 0 {
		fmt.Fprintln(os.Stderr, "invalid drill profile")
		os.Exit(2)
	}
	exitCode := func() int {
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		r := report{Schema: "makewand.server-drill.v1", Started: time.Now().UTC(), Status: "failed", Environment: map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version(), "cpus": runtime.NumCPU()}, Profile: map[string]any{"requests": o.Requests, "concurrency": o.Concurrency, "tenants": o.Tenants, "router_calls": o.RouterCalls}, Scope: "Local loopback HTTP and real SQLite/process recovery under a deterministic provider. Measures this machine; excludes production deployment, external providers, hardware failure, and certification."}
		if err := runDrill(ctx, o, &r); err != nil {
			r.Error = err.Error()
		} else {
			r.Status = "passed"
		}
		r.Finished = time.Now().UTC()
		if err := os.MkdirAll(filepath.Dir(o.Output), 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		data, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(o.Output, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("server drill %s: HTTP p50=%.2fms p95=%.2fms throughput=%.1f/s unexpected_error_rate=%.4f RPO_acknowledged_lost=%d RTO=%.2fms; report=%s\n", r.Status, r.HTTP.P50MS, r.HTTP.P95MS, r.HTTP.RPS, r.HTTP.ErrorRate, r.Recovery.RPOAcknowledgedLost, r.Recovery.RTOMS, o.Output)
		if r.Status != "passed" {
			fmt.Fprintln(os.Stderr, r.Error)
			return 1
		}
		return 0
	}()
	os.Exit(exitCode)
}

func record(r *report, name string, passed bool, detail string) error {
	r.Checks = append(r.Checks, check{Name: name, Passed: passed, Detail: detail})
	if !passed {
		return fmt.Errorf("%s: %s", name, detail)
	}
	return nil
}

type localProvider struct {
	active          atomic.Int64
	peak            atomic.Int64
	calls           atomic.Int64
	bulkheadRelease chan struct{}
	releaseOnce     sync.Once
	cancelStarted   sync.Map
	Identity        string
	Delay           time.Duration
}

func (p *localProvider) Name() string      { return "claude" }
func (p *localProvider) IsAvailable() bool { return true }
func (p *localProvider) enter() {
	active := p.active.Add(1)
	for previous := p.peak.Load(); active > previous; previous = p.peak.Load() {
		if p.peak.CompareAndSwap(previous, active) {
			break
		}
	}
}
func (p *localProvider) Chat(ctx context.Context, messages []router.Message, _ string, _ int) (string, router.Usage, error) {
	p.enter()
	defer p.active.Add(-1)
	p.calls.Add(1)
	content := ""
	if len(messages) > 0 {
		content = messages[len(messages)-1].Content
	}
	if strings.HasPrefix(content, "slow:cancel:") {
		p.cancelStarted.Store(content, true)
		<-ctx.Done()
		return "", router.Usage{MeasuredCost: true}, ctx.Err()
	}
	if content == "slow:crash" {
		<-ctx.Done()
		return "", router.Usage{MeasuredCost: true}, ctx.Err()
	}
	if content == "hold:bulkhead" {
		select {
		case <-p.bulkheadRelease:
		case <-ctx.Done():
			return "", router.Usage{MeasuredCost: true}, ctx.Err()
		}
	}
	delay := p.Delay
	if strings.HasPrefix(content, "slow:") {
		delay = 500 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return "", router.Usage{MeasuredCost: true}, ctx.Err()
	case <-time.After(delay):
	}
	return p.Identity + content, router.Usage{Provider: "claude", Model: "local-drill", InputTokens: 4, OutputTokens: 4, Cost: .01}, nil
}
func (p *localProvider) ChatStream(ctx context.Context, messages []router.Message, _ string, _ int) (<-chan router.StreamChunk, error) {
	out := make(chan router.StreamChunk, 1)
	p.enter()
	go func() {
		defer p.active.Add(-1)
		defer close(out)
		select {
		case out <- router.StreamChunk{Content: "local-stream"}:
		case <-ctx.Done():
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		select {
		case out <- router.StreamChunk{Done: true}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

func seedState(path string, tenants, calls int) (*serverauth.SQLiteStore, *serverteam.SQLiteStore, *serverusage.SQLiteStore, error) {
	tokens, err := serverauth.OpenSQLiteStore(path)
	if err != nil {
		return nil, nil, nil, err
	}
	teams, err := serverteam.OpenSQLiteStore(path)
	if err != nil {
		tokens.Close()
		return nil, nil, nil, err
	}
	usage, err := serverusage.OpenSQLiteStore(path)
	if err != nil {
		tokens.Close()
		teams.Close()
		return nil, nil, nil, err
	}
	existing := tokens.TokenRules()
	if len(existing) == 0 {
		for i := 0; i < tenants; i++ {
			id := strconv.Itoa(i)
			cap := float64(calls) * .01
			org, err := teams.CreateOrganization(serverteam.Organization{ID: "org-" + id, Name: "Tenant " + id, MonthlyBudgetUSD: cap})
			if err != nil {
				return tokens, teams, usage, err
			}
			_, err = teams.CreateProject(serverteam.Project{ID: "project-" + id, OrganizationID: org.ID, Name: "Project " + id, MonthlyBudgetUSD: cap})
			if err != nil {
				return tokens, teams, usage, err
			}
			_, _, err = tokens.Issue(serverauth.TokenRule{ID: "token-" + id, Token: "drill-token-" + id, OrganizationID: org.ID, ProjectID: "project-" + id, Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerDay: cap, MaxCostUSDPerMonth: cap})
			if err != nil {
				return tokens, teams, usage, err
			}
		}
		_, _, err = tokens.Issue(serverauth.TokenRule{ID: "control", Token: "drill-control", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerMonth: 1})
		if err != nil {
			return tokens, teams, usage, err
		}
	}
	return tokens, teams, usage, nil
}

func serveChild(o options) error {
	state := filepath.Join(o.StateDir, "state.db")
	opts := backup.Options{StateDBPath: state, AuthConfigPath: filepath.Join(o.StateDir, "server_auth.json")}
	if err := backup.RecoverRestore(opts); err != nil {
		return err
	}
	tokens, teams, usage, err := seedState(state, o.Tenants, o.BudgetCalls)
	if err != nil {
		return err
	}
	defer tokens.Close()
	defer teams.Close()
	defer usage.Close()
	provider := &localProvider{Delay: 2 * time.Millisecond, bulkheadRelease: make(chan struct{})}
	rtr, err := router.NewRouterFromConfig(router.RouterConfig{Providers: map[string]router.ProviderEntry{"claude": {Provider: provider, Access: router.AccessAPI}}, DefaultModel: "claude", CodingModel: "claude"})
	if err != nil {
		return err
	}
	var draining atomic.Bool
	handler := rtr.HTTPHandler(router.HTTPHandlerOptions{Authorizer: tokens, UsageLogger: usage, UsageReader: usage, TeamStore: teams, BudgetReservationUSD: .01, BudgetReserver: usage, StrictAccounting: true, UsageLogErrorHandler: func(err error) { fmt.Fprintln(os.Stderr, "accounting:", err) }, ReadinessCheck: func(context.Context) error {
		if draining.Load() {
			return fmt.Errorf("draining")
		}
		return nil
	}})
	mux := http.NewServeMux()
	var cancelCompleted sync.Map
	mux.HandleFunc("/drill/inflight", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{"active": provider.active.Load(), "peak": provider.peak.Load(), "calls": provider.calls.Load()})
	})
	mux.HandleFunc("/drill/release-bulkhead", func(w http.ResponseWriter, _ *http.Request) {
		provider.releaseOnce.Do(func() { close(provider.bulkheadRelease) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/drill/cancellation", func(w http.ResponseWriter, req *http.Request) {
		id := req.URL.Query().Get("request_id")
		_, started := provider.cancelStarted.Load(id)
		_, completed := cancelCompleted.Load(id)
		_ = json.NewEncoder(w).Encode(map[string]bool{"started": started, "completed": completed})
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := serverhttp.RequestIDFromRequest(req)
		if strings.HasPrefix(id, "slow:cancel:") {
			// Router's usage finalizer runs before ServeHTTP returns. A client
			// transport cancellation or provider exit alone does not prove that
			// the durable reservation has finished settling.
			defer cancelCompleted.Store(id, true)
		}
		handler.ServeHTTP(w, req)
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := http.Server{Handler: serverhttp.WithRequestID(mux), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Second}
	if err = os.WriteFile(o.ReadyFile, []byte("http://"+listener.Addr().String()), 0o600); err != nil {
		listener.Close()
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-signals:
		case <-stop:
			return
		}
		draining.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			server.Close()
		}
		close(done)
	}()
	err = server.Serve(listener)
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return tokens.PersistUsageCounters()
}

func restoreChild(o options) error {
	opts := backup.Options{StateDBPath: filepath.Join(o.StateDir, "state.db"), AuthConfigPath: filepath.Join(o.StateDir, "server_auth.json"), Progress: func(phase, component string) {
		if phase == "installed" {
			if err := os.WriteFile(o.ReadyFile, []byte(component), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			select {}
		}
	}}
	_, err := backup.Restore(o.Archive, opts)
	return err
}

type childProcess struct {
	cmd    *exec.Cmd
	url    string
	log    *os.File
	waited bool
}

func startChild(ctx context.Context, o options, dir string) (*childProcess, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ready := filepath.Join(dir, "ready-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	log, err := os.CreateTemp(dir, "process-*.log")
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, exe, "--child", "--state-dir", dir, "--ready-file", ready, "--tenants", strconv.Itoa(o.Tenants), "--budget-calls", strconv.Itoa(o.BudgetCalls))
	command.Stdout = log
	command.Stderr = log
	if err = command.Start(); err != nil {
		log.Close()
		return nil, err
	}
	child := &childProcess{cmd: command, log: log}
	readyCtx, cancelReady := context.WithTimeout(ctx, 30*time.Second)
	defer cancelReady()
	if err = waitForFile(readyCtx, ready); err != nil {
		child.kill()
		return nil, err
	}
	data, err := os.ReadFile(ready)
	if err != nil {
		child.kill()
		return nil, err
	}
	child.url = string(data)
	return child, nil
}
func (c *childProcess) kill() {
	if c == nil || c.waited {
		return
	}
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	c.log.Close()
	c.waited = true
}
func (c *childProcess) graceful() error {
	if c.waited {
		return nil
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	err := c.cmd.Wait()
	c.log.Close()
	c.waited = true
	return err
}
func waitForFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func waitActive(ctx context.Context, client *http.Client, url string) error {
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/drill/inflight", nil)
		response, err := client.Do(request)
		if err == nil {
			var state map[string]int64
			decodeErr := json.NewDecoder(response.Body).Decode(&state)
			response.Body.Close()
			if decodeErr == nil && state["active"] > 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func post(ctx context.Context, client *http.Client, url, token, content string, stream bool, requestID ...string) (int, string, error) {
	payload, _ := json.Marshal(map[string]any{"model": "claude", "messages": []map[string]string{{"role": "user", "content": content}}, "stream": stream})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if len(requestID) != 0 {
		request.Header.Set(serverhttp.HeaderRequestID, requestID[0])
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, string(data), err
}

func waitCancellation(ctx context.Context, client *http.Client, baseURL, id string, completed bool) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/drill/cancellation", nil)
		if err != nil {
			return err
		}
		query := request.URL.Query()
		query.Set("request_id", id)
		request.URL.RawQuery = query.Encode()
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		var state struct {
			Started   bool `json:"started"`
			Completed bool `json:"completed"`
		}
		err = json.NewDecoder(response.Body).Decode(&state)
		response.Body.Close()
		if err != nil {
			return err
		}
		if state.Completed && !state.Started {
			return fmt.Errorf("cancellation %s completed without dispatching the provider", id)
		}
		if state.Started && (!completed || state.Completed) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for cancellation %s completed=%t: %w", id, completed, ctx.Err())
		case <-ticker.C:
		}
	}
}

func runHTTP(ctx context.Context, o options, client *http.Client, url string) (latency, error) {
	start := time.Now()
	durations := make([]float64, o.Requests)
	jobs := make(chan int)
	var successful, rejected, unexpected atomic.Int64
	var sampleMu sync.Mutex
	var samples []string
	sample := func(status int, body string, err error) {
		sampleMu.Lock()
		defer sampleMu.Unlock()
		if len(samples) < 8 {
			samples = append(samples, fmt.Sprintf("status=%d body=%s err=%v", status, body, err))
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < o.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				tenant := index % o.Tenants
				content := fmt.Sprintf("tenant-%d:load-%d", tenant, index)
				begin := time.Now()
				status, body, err := post(ctx, client, url, "drill-token-"+strconv.Itoa(tenant), content, false)
				durations[index] = float64(time.Since(begin).Microseconds()) / 1000
				switch {
				case err != nil:
					sample(status, body, err)
					unexpected.Add(1)
				case status == http.StatusOK && strings.Contains(body, content):
					successful.Add(1)
				case status == http.StatusTooManyRequests:
					rejected.Add(1)
				default:
					sample(status, body, err)
					unexpected.Add(1)
				}
			}
		}()
	}
	for i := 0; i < o.Requests; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return latency{}, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	sort.Float64s(durations)
	elapsed := time.Since(start)
	m := latency{Requests: o.Requests, Successful: int(successful.Load()), ExpectedRejected: int(rejected.Load()), Unexpected: int(unexpected.Load()), P50MS: durations[(len(durations)-1)/2], P95MS: durations[(len(durations)-1)*95/100], RPS: float64(o.Requests) / elapsed.Seconds(), ErrorRate: float64(unexpected.Load()) / float64(o.Requests), DurationMS: elapsed.Milliseconds(), ErrorSamples: samples}
	return m, nil
}

// runHTTPBulkhead holds real HTTP provider calls at a barrier until every
// configured slot is occupied. This exercises admission independently of the
// SQLite write speed; the normal 2 ms stub can finish before the next writer.
func runHTTPBulkhead(ctx context.Context, o options, client *http.Client, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	workers := min(o.Concurrency, 16)
	wantActive := min(workers, 8)
	jobs := make(chan struct{}, 24)
	for i := 0; i < cap(jobs); i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				status, body, err := post(ctx, client, url, "drill-control", "hold:bulkhead", false)
				if err != nil || status != http.StatusOK || !strings.Contains(body, "hold:bulkhead") {
					errs <- fmt.Errorf("bulkhead burst: status=%d body=%s err=%v", status, body, err)
					return
				}
			}
		}()
	}
	finish := func(err error) error {
		cancel()
		wg.Wait()
		return err
	}
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/drill/inflight", nil)
		response, err := client.Do(request)
		if err != nil {
			return finish(err)
		}
		var state map[string]int64
		err = json.NewDecoder(response.Body).Decode(&state)
		response.Body.Close()
		if err != nil {
			return finish(err)
		}
		if state["active"] >= int64(wantActive) {
			break
		}
		select {
		case err := <-errs:
			return finish(err)
		case <-ctx.Done():
			return finish(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/drill/release-bulkhead", nil)
	response, err := client.Do(request)
	if err != nil {
		return finish(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return finish(fmt.Errorf("bulkhead barrier release: status=%d", response.StatusCode))
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

func runDrill(ctx context.Context, o options, r *report) error {
	directory, err := os.MkdirTemp("", "makewand-server-drill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	o.BudgetCalls = o.Requests / o.Tenants / 2
	child, err := startChild(ctx, o, directory)
	if err != nil {
		return err
	}
	defer child.kill()
	transport := &http.Transport{MaxIdleConns: o.Concurrency * 2, MaxIdleConnsPerHost: o.Concurrency * 2}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	r.HTTP, err = runHTTP(ctx, o, client, child.url)
	if r.HTTP.Unexpected > 0 {
		data, _ := os.ReadFile(child.log.Name())
		if r.Environment == nil {
			r.Environment = make(map[string]any)
		}
		r.Environment["accounting_diagnostics"] = string(data)
	}
	if err != nil {
		return err
	}
	if err = record(r, "http_multi_tenant_budget", r.HTTP.Unexpected == 0 && r.HTTP.Successful == o.BudgetCalls*o.Tenants, fmt.Sprintf("success=%d expected=%d rejected=%d", r.HTTP.Successful, o.BudgetCalls*o.Tenants, r.HTTP.ExpectedRejected)); err != nil {
		return err
	}
	if err = runHTTPBulkhead(ctx, o, client, child.url); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, child.url+"/drill/inflight", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	var providerStats map[string]int64
	err = json.NewDecoder(response.Body).Decode(&providerStats)
	response.Body.Close()
	if err != nil {
		return err
	}
	r.HTTP.ProviderPeak = providerStats["peak"]
	if err = record(r, "http_provider_bulkhead", r.HTTP.ProviderPeak == int64(min(o.Concurrency, 8)), fmt.Sprintf("24 barrier-held HTTP calls; peak=%d default API limit=8", r.HTTP.ProviderPeak)); err != nil {
		return err
	}
	path := filepath.Join(directory, "state.db")
	db, err := serverdb.Open(path)
	if err != nil {
		return err
	}
	var mismatches int
	if err = db.QueryRow(`SELECT COUNT(*) FROM usage_entries WHERE token_id LIKE 'token-%' AND (organization_id!='org-'||substr(token_id,7) OR project_id!='project-'||substr(token_id,7))`).Scan(&mismatches); err != nil {
		db.Close()
		return err
	}
	if err = record(r, "tenant_attribution", mismatches == 0, fmt.Sprintf("mismatched ledger rows=%d", mismatches)); err != nil {
		db.Close()
		return err
	}
	db.Close()
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("slow:cancel:%d", i)
		cancelCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = post(cancelCtx, client, child.url, "drill-control", id, false, id)
		}()
		if err = waitCancellation(ctx, client, child.url, id, false); err != nil {
			cancel()
			return err
		}
		cancel()
		<-done
		if err = waitCancellation(ctx, client, child.url, id, true); err != nil {
			return err
		}
	}
	db, err = serverdb.Open(path)
	if err != nil {
		return err
	}
	var pending, refunded int
	err = db.QueryRow(`SELECT COUNT(*) FROM budget_reservations r JOIN budget_reservation_scopes s ON s.request_id=r.request_id WHERE s.scope_id='control' AND r.state='pending'`).Scan(&pending)
	if err == nil {
		err = db.QueryRow(`SELECT COUNT(*) FROM usage_entries WHERE token_id='control' AND request_id LIKE 'slow:cancel:%' AND cost_micro_usd=0`).Scan(&refunded)
	}
	db.Close()
	if err != nil {
		return err
	}
	if err = record(r, "cancel_refunds_known_unspent", pending == 0 && refunded == 8, fmt.Sprintf("8 distinct providers started and HTTP handlers settled; pending control reservations=%d zero-cost refunds=%d", pending, refunded)); err != nil {
		return err
	}
	if err = backupDrill(ctx, o, r, child, directory, client); err != nil {
		return err
	}
	// Kill a real child with an admitted provider request, then restart from WAL.
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		_, _, _ = post(ctx, client, child.url, "drill-control", "slow:crash", false)
	}()
	if err = waitActive(ctx, client, child.url); err != nil {
		return err
	}
	child.kill()
	<-requestDone
	start := time.Now()
	child, err = startChild(ctx, o, directory)
	if err != nil {
		return err
	}
	defer child.kill()
	r.Recovery.CrashRestartMS = float64(time.Since(start).Microseconds()) / 1000
	db, err = serverdb.Open(path)
	if err != nil {
		return err
	}
	var uncertain int
	err = db.QueryRow(`SELECT COUNT(*) FROM budget_reservations r JOIN budget_reservation_scopes s ON s.request_id=r.request_id WHERE s.scope_id='control' AND r.state='pending'`).Scan(&uncertain)
	db.Close()
	if err != nil {
		return err
	}
	if err = record(r, "crash_preserves_uncertain_cost", uncertain > 0, fmt.Sprintf("durable uncertain reservations=%d", uncertain)); err != nil {
		return err
	}
	// Outstanding settled tenant spend must still deny after a process restart.
	status, _, err := post(ctx, client, child.url, "drill-token-0", "after-restart", false)
	if err != nil {
		return err
	}
	if err = record(r, "restart_keeps_budget", status == http.StatusTooManyRequests, fmt.Sprintf("HTTP status=%d", status)); err != nil {
		return err
	}
	streamDone := make(chan error, 1)
	go func() {
		status, body, err := post(ctx, client, child.url, "drill-control", "stream:shutdown", true)
		if err == nil && (status != http.StatusOK || !strings.Contains(body, "[DONE]")) {
			err = fmt.Errorf("stream not drained: status=%d body=%s", status, body)
		}
		streamDone <- err
	}()
	if err = waitActive(ctx, client, child.url); err != nil {
		return err
	}
	start = time.Now()
	if err = child.graceful(); err != nil {
		return err
	}
	r.Recovery.GracefulDrainMS = float64(time.Since(start).Microseconds()) / 1000
	if err = <-streamDone; err != nil {
		return err
	}
	if err = record(r, "sigterm_drains_stream", time.Since(start) < 30*time.Second, fmt.Sprintf("drain %.2f ms", r.Recovery.GracefulDrainMS)); err != nil {
		return err
	}
	if o.RouterCalls > 0 {
		if err = routerIsolation(ctx, o.RouterCalls); err != nil {
			return err
		}
		if err = record(r, "router_identity_isolation", true, fmt.Sprintf("50 routers / %d calls", o.RouterCalls)); err != nil {
			return err
		}
	}
	return nil
}

func hashString(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func routerIsolation(ctx context.Context, calls int) error {
	const count = 50
	routers := make([]*router.Router, count)
	for i := range routers {
		identity := fmt.Sprintf("router-%d:", i)
		r, err := router.NewRouterFromConfig(router.RouterConfig{Providers: map[string]router.ProviderEntry{"claude": {Provider: &localProvider{Identity: identity}, Access: router.AccessAPI}}, DefaultModel: "claude"})
		if err != nil {
			return err
		}
		routers[i] = r
	}
	jobs := make(chan int)
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				index := job % count
				message := fmt.Sprintf("call-%d", job)
				content, _, _, err := routers[index].ChatWith(ctx, "claude", router.PhaseCode, []router.Message{{Role: "user", Content: message}}, "")
				want := fmt.Sprintf("router-%d:%s", index, message)
				if err != nil || content != want {
					select {
					case errs <- fmt.Errorf("identity mismatch: %s want %s err %v", content, want, err):
					default:
					}
					return
				}
			}
		}()
	}
	for i := 0; i < calls; i++ {
		select {
		case jobs <- i:
		case err := <-errs:
			close(jobs)
			wg.Wait()
			return err
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}
