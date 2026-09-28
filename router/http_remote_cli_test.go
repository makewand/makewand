package router

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	binDir := t.TempDir()
	script := filepath.Join(binDir, "codex")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo codex-fake; exit 0; fi\n" +
		"f=$(mktemp \"" + logDir + "/call.XXXXXX\")\n" +
		"pwd -P > \"$f.dir\"\n" +
		"for a in \"$@\"; do printf '%s" + strings.ReplaceAll(fakeCodexArgSep, "\n", "\\n") + "' \"$a\" >> \"$f.args\"; done\n" +
		"rm -f \"$f\"\n" +
		"echo '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"FAKE-CODEX-ANSWER\"}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture script must be executable.
		t.Fatalf("WriteFile(fake codex): %v", err)
	}
	return script
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
	r, err := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"codex": {Provider: NewCodexCLI(codexBin), Access: AccessSubscription},
		},
		UsageMode: "balanced",
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
			logDir := t.TempDir()
			codex := writeRecordingCodexCLI(t, logDir)
			r := newCodexOnlyHTTPRouter(t, codex)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			r.HTTPHandler().ServeHTTP(rec, req)
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
