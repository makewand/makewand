package router

import (
	"github.com/makewand/makewand/internal/testfixture"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeCodexInvocation is one recorded exec of the fake codex CLI.
type fakeCodexInvocation struct {
	dir  string
	args []string
}

const fakeCodexArgSep = "\n----ARG----\n"

// writeRecordingCodexCLI writes a fake `codex` that records its working
// directory and argv (one file per invocation under logDir) and answers with a
// codex `exec --json` agent_message. It never contacts a real model.
func writeRecordingCodexCLI(t *testing.T, logDir string) string {
	t.Helper()
	return testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "record-codex", LogDir: logDir})
}

func readFakeCodexInvocations(t *testing.T, logDir string) []fakeCodexInvocation {
	t.Helper()
	dirFiles, err := filepath.Glob(filepath.Join(logDir, "call.*.dir"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	var out []fakeCodexInvocation
	for _, df := range dirFiles {
		dirRaw, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", df, err)
		}
		argsRaw, err := os.ReadFile(strings.TrimSuffix(df, ".dir") + ".args")
		if err != nil {
			t.Fatalf("ReadFile(args for %s): %v", df, err)
		}
		args := strings.Split(strings.TrimSuffix(string(argsRaw), fakeCodexArgSep), fakeCodexArgSep)
		out = append(out, fakeCodexInvocation{dir: strings.TrimSpace(string(dirRaw)), args: args})
	}
	return out
}

func newCodexOnlyHTTPRouter(t *testing.T, codexBin string) *Router {
	t.Helper()
	configDir := t.TempDir()
	// This fixture registers only Codex. Make it an actual review generator,
	// rather than the default ensemble's judge, and keep adaptive fallback in
	// the same registered-provider scope. A sole result needs no judge call.
	overrides := `{"strategies":{"balanced":{"review":{"tier":"mid","providers":["codex"]}}},"build_strategies":{"balanced":{"review":{"primary":"codex","fallbacks":[]}},"power":{"review":{"primary":"codex","fallbacks":[]}}},"power_ensemble":{"review":{"generators":["codex"],"judge":"claude"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "routing.json"), []byte(overrides), 0o600); err != nil {
		t.Fatalf("WriteFile(routing.json): %v", err)
	}
	r, err := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"codex": {Provider: NewCodexCLI(codexBin), Access: AccessSubscription},
		},
		UsageMode: "balanced",
		ConfigDir: configDir,
	})
	if err != nil {
		t.Fatalf("NewRouterFromConfig: %v", err)
	}
	return r
}

// TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd reproduces
// go-router#1 / replay-0926-memory#14: a remote HTTP request classified as
// TaskReview used to run `codex review --uncommitted` (dropping the caller's
// prompt) in the serve process cwd, returning a review of the host's
// uncommitted diff to the remote caller.
func TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd(t *testing.T) {
	serverCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(serverCwd); err == nil {
		serverCwd = resolved
	}

	cases := []struct {
		name   string
		path   string
		body   string
		prompt string
		stream bool
	}{
		{
			name:   "auto-classified review",
			path:   "/v1/chat/completions",
			body:   `{"messages":[{"role":"user","content":"please review the code quality of my handler"}]}`,
			prompt: "please review the code quality of my handler",
		},
		{
			name:   "openai client default model alias with /review",
			path:   "/v1/chat/completions",
			body:   `{"model":"gpt-4o","messages":[{"role":"user","content":"/review something for me"}]}`,
			prompt: "/review something for me",
		},
		{
			name:   "power mode chinese review with explicit codex",
			path:   "/v1/chat/completions",
			body:   `{"model":"codex","mode":"power","messages":[{"role":"user","content":"审查一下这段代码: func f() {}"}]}`,
			prompt: "审查一下这段代码: func f() {}",
		},
		{
			name:   "power mode ensemble without explicit model",
			path:   "/v1/chat/completions",
			body:   `{"mode":"power","messages":[{"role":"user","content":"review this diff please"}]}`,
			prompt: "review this diff please",
		},
		{
			name:   "responses api review",
			path:   "/v1/responses",
			body:   `{"model":"codex","input":"please review my function"}`,
			prompt: "please review my function",
		},
		{
			name:   "streaming review",
			path:   "/v1/chat/completions",
			body:   `{"model":"codex","stream":true,"messages":[{"role":"user","content":"review the stream path"}]}`,
			prompt: "review the stream path",
			stream: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpBase := t.TempDir()
			t.Setenv("TMPDIR", tmpBase)
			t.Setenv("TMP", tmpBase)
			t.Setenv("TEMP", tmpBase)
			t.Setenv("SystemTemp", tmpBase)
			logDir := t.TempDir()
			codex := writeRecordingCodexCLI(t, logDir)
			r := newCodexOnlyHTTPRouter(t, codex)
			var traceMu sync.Mutex
			var traces []TraceEvent
			r.SetTraceSink(TraceSinkFunc(func(event TraceEvent) {
				traceMu.Lock()
				traces = append(traces, event)
				traceMu.Unlock()
			}))

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			r.HTTPHandler().ServeHTTP(rec, req)
			if tc.name == "power mode ensemble without explicit model" {
				traceMu.Lock()
				started, codexAttempt := false, false
				for _, event := range traces {
					if event.Event == "ensemble_start" && event.Phase == "review" {
						started = true
					}
					if event.Event == "ensemble_generator_success" && event.Selected == "codex" {
						codexAttempt = runtime.GOOS != "windows"
					}
					if event.Event == "ensemble_generator_error" && event.Selected == "codex" && strings.Contains(event.Error, "bubblewrap") {
						codexAttempt = runtime.GOOS == "windows"
					}
					if strings.HasPrefix(event.Event, "judge_") && event.Event != "judge_skipped_single_result" {
						traceMu.Unlock()
						t.Fatalf("single-provider fixture invoked an ensemble judge: %+v", event)
					}
				}
				traceMu.Unlock()
				if !started || !codexAttempt {
					t.Fatalf("power review did not exercise the Codex ensemble: started=%t codexAttempt=%t traces=%+v", started, codexAttempt, traces)
				}
			}
			if runtime.GOOS == "windows" {
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "bubblewrap") {
					t.Fatalf("Windows remote CLI must refuse unavailable Linux isolation: status=%d body=%s", rec.Code, rec.Body.String())
				}
				if calls := readFakeCodexInvocations(t, logDir); len(calls) != 0 {
					t.Fatalf("rejected remote request executed %d fixture calls", len(calls))
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "FAKE-CODEX-ANSWER") {
				t.Fatalf("response does not carry the codex answer: %s", rec.Body.String())
			}

			calls := readFakeCodexInvocations(t, logDir)
			if len(calls) == 0 {
				t.Fatal("fake codex was never invoked")
			}
			for _, call := range calls {
				joined := strings.Join(call.args, " ")
				if strings.Contains(joined, "review --uncommitted") || (len(call.args) > 0 && call.args[0] == "review") {
					t.Fatalf("remote request ran host-state-dependent `codex %s`", joined)
				}
				if len(call.args) == 0 || call.args[0] != "exec" {
					t.Fatalf("codex argv = %q, want an `exec` invocation", call.args)
				}
				if !strings.Contains(joined, tc.prompt) {
					t.Fatalf("caller prompt %q was dropped; argv = %q", tc.prompt, call.args)
				}
				for i, a := range call.args {
					if a == "--sandbox" && i+1 < len(call.args) && call.args[i+1] != "read-only" {
						t.Fatalf("remote codex sandbox = %q, want read-only", call.args[i+1])
					}
				}
				if call.dir == serverCwd {
					t.Fatalf("codex ran in the server process cwd %q", call.dir)
				}
				resolvedBase, _ := filepath.EvalSymlinks(tmpBase)
				if !strings.HasPrefix(call.dir, resolvedBase+string(os.PathSeparator)) {
					t.Fatalf("codex ran in %q, want a per-request temp dir under %q", call.dir, resolvedBase)
				}
				if _, err := os.Stat(call.dir); !os.IsNotExist(err) {
					t.Fatalf("per-request CLI temp dir %q was not removed (stat err = %v)", call.dir, err)
				}
			}
		})
	}
}
