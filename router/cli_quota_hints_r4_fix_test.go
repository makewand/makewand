package router

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/testfixture"
)

// Round 4 of the provider CLI quota classification (router/cli.go, the
// catalog in cli_message_catalog.json):
//
//  1. custom-cli-quota: a CLI the catalog does not know (NewCommandCLI) seals
//     its pool on a quota or rate-limit refusal its error report starts with,
//     read with generic anchored patterns; task content still never does.
//  2. gemini-legacy: the legacy gemini-cli transport reads Gemini CLI's own
//     entry (its pretty-printed JSON error report, "Usage limit reached for
//     <model>."), agy - registered under the same router name - agy's.
//  3. quota-memory: classifying stderr costs a bounded amount whatever its
//     size: only its last lines are read, and what precedes them still
//     decides what is task content (an open fence, a transcript section, the
//     prompt echo).
//  4. codex-prompt-echo: codex's echo of the prompt is never codex's own
//     message, on its streaming path too.

var errR4Exit = errors.New("exit status 1")

func r4Sealed(err error) bool {
	var perr *ProviderError
	return errors.As(err, &perr) && perr.Kind == ErrorKindRateLimit && perr.Retryable && perr.StatusCode == 429
}

func TestCustomCLIRefusalSealsWithGenericPatterns_R4(t *testing.T) {
	for _, stderr := range []string{
		"quota exceeded",
		"quota exceeded\n",
		"Error: quota exceeded",
		"Error: Quota exhausted for this API key.",
		"private: usage limit reached",
		"Weekly usage limit reached · resets Monday",
		"Rate limit exceeded",
		"rate limited, try again in 30s",
		"Error: 429 Too Many Requests",
		"Too many requests",
		"HTTP 429",
		"error: insufficient_quota",
		"litellm.RateLimitError: AnthropicException - rate limit reached for requests",
		"You have exhausted your quota on this model.",
		"You've hit your limit · resets 8pm",
		"You're out of credits.",
		"Error: RESOURCE_EXHAUSTED",
		"Error code: 429 - {'error': {'type': 'rate_limit_error'}}",
		"Loaded config\nconnecting...\nError: request failed: rate limit exceeded\n",
		"[ERROR] usage limit reached\nexiting\n",
		"\x1b[31m✖ Quota exceeded\x1b[0m\r\n",
	} {
		if err := formatCLIExecutionError("private", "", stderr, errR4Exit, nil, time.Second); !r4Sealed(err) {
			t.Errorf("custom CLI refusal %q classified as %v", stderr, err)
		}
		if !looksLikeQuotaExhaustion("private", stderr) {
			t.Errorf("custom CLI refusal %q not detected", stderr)
		}
	}
	for _, stderr := range []string{
		"",
		"Error: build failed",
		"    quota exceeded",
		"\tRate limit exceeded",
		"429 passed",
		"===== 429 passed in 3.2s =====",
		"line 429",
		"Rate limit exceeded: backing off\nreconnected\nError: build failed\n",
		"The quota exceeded banner is now localized.",
		"Here is the handler:\n```\nquota exceeded\n```\nDone.",
		"Added a rate limit to the login endpoint; resets at midnight",
		"stream closed unexpectedly: Transport error (1007)",
	} {
		if err := formatCLIExecutionError("private", "", stderr, errR4Exit, nil, time.Second); ErrorKindOf(err) == ErrorKindRateLimit {
			t.Errorf("custom CLI task content %q sealed the pool: %v", stderr, err)
		}
	}
}

// The real process path: a custom command whose first run is refused retries
// once (the retryable refusal) and succeeds.
func TestCustomCLIRefusalIsRetried_R4(t *testing.T) {
	for _, first := range []string{"Error: rate limit exceeded", "usage limit reached (429)", "quota exceeded"} {
		state := t.TempDir() + "/count"
		script := testfixture.WriteCLI(t, "custom", testfixture.Spec{Mode: "counter", StateFile: state, FirstError: first, Stdout: "ok after retry\n"})
		content, _, err := NewCommandCLI("custom", script, []string{state}, PromptModeStdin).Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 64)
		if err != nil || !strings.Contains(content, "ok after retry") {
			t.Errorf("first refusal %q: Chat = %q, %v; want the retry's answer", first, content, err)
		}
	}
}

// Gemini CLI's JSON error report as --output-format json prints it on stderr.
const r4GeminiTerminalQuota = "{\n  \"session_id\": \"0d6f\",\n  \"error\": {\n    \"type\": \"TerminalQuotaError\",\n    \"message\": \"You have exhausted your daily quota on this model.\",\n    \"code\": 429\n  }\n}\n"

func TestGeminiCLIRefusalsSealTheLegacyTransport_R4(t *testing.T) {
	refusals := []string{
		r4GeminiTerminalQuota,
		"{\n  \"error\": {\n    \"type\": \"Error\",\n    \"message\": \"[API Error: Resource exhausted. (Status: RESOURCE_EXHAUSTED)]\",\n    \"code\": 429\n  }\n}\n",
		"Loaded cached credentials.\n[ERROR] {\n  \"error\": {\n    \"type\": \"RetryableQuotaError\",\n    \"code\": 429\n  }\n}\n",
		"Usage limit reached for gemini-2.5-pro.\nAccess resets at 3:00 PM PST.\n/stats model for usage details\n",
		"[API Error: You have exhausted your daily quota on this model.]\n",
	}
	for _, stderr := range refusals {
		// formatCLIExecutionError with the router name reads Gemini CLI's entry.
		if err := formatCLIExecutionError("gemini", "", stderr, errR4Exit, nil, time.Second); !r4Sealed(err) {
			t.Errorf("gemini %q classified as %v", stderr, err)
		}
		script := testfixture.WriteCLI(t, "gemini", testfixture.Spec{Mode: "output", Stderr: stderr, ExitCode: 1})
		_, _, err := NewGeminiCLI(script).Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 64)
		if ErrorKindOf(err) != ErrorKindRateLimit {
			t.Errorf("gemini-cli transport: %q classified as %v", stderr, err)
		}
	}
	// agy, registered under the same router name, keeps its own formats.
	agyOnly := "Error: Individual quota reached.\n"
	script := testfixture.WriteCLI(t, "agy", testfixture.Spec{Mode: "output", Stderr: agyOnly, ExitCode: 1})
	if _, _, err := NewAgyCLI(script).Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 64); ErrorKindOf(err) != ErrorKindRateLimit {
		t.Errorf("agy transport: %q classified as %v", agyOnly, err)
	}
	if _, _, err := NewGeminiCLI(script).Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 64); ErrorKindOf(err) == ErrorKindRateLimit {
		t.Errorf("gemini-cli transport sealed on agy's format %q: %v", agyOnly, err)
	}
	// Task content in a JSON report is not a refusal.
	for _, stderr := range []string{
		"{\n  \"error\": {\n    \"type\": \"FatalToolExecutionError\",\n    \"message\": \"429 passed\",\n    \"code\": 1\n  }\n}\n",
		"The report looked like this:\n  {\n    \"code\": 429\n  }\n",
	} {
		if err := formatCLIExecutionError("gemini", "", stderr, errR4Exit, nil, time.Second); ErrorKindOf(err) == ErrorKindRateLimit {
			t.Errorf("gemini task content %q sealed the pool: %v", stderr, err)
		}
	}
}

// Classifying even the largest stderr the capture keeps (10 MiB) is cheap,
// and a refusal the CLI ends with is still found.
//
// The allocation bounds are checked in normal builds only. Under -race the
// runtime drops part of what is put back into a sync.Pool, so a regexp match
// may allocate afresh the matcher state the regexp package pools (about 40
// KiB for each catalog pattern the tail's lines are tried against): hundreds
// of MiB here, with the same work done and the same verdicts. A race build
// still checks the verdicts and the time, with raceSlowdown more room.
func TestCLIQuotaClassificationCostIsBounded_R4(t *testing.T) {
	const size = 10 << 20
	ends := map[string]string{
		"claude": "You've hit your limit · resets 8pm\n",
		"codex":  "ERROR: You've hit your usage limit. Try again at 8:00 PM.\n",
		"gemini": r4GeminiTerminalQuota,
		"agy":    "Error: You have exhausted your quota on this model.\n",
	}
	for _, unit := range []string{"x\n", "Error: x\n", "codex\nERROR: x\n", "```\nx\n"} {
		filler := strings.Repeat(unit, size/len(unit))
		for cli, refusal := range ends {
			refused := filler + "\n" + refusal
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			fillerSealed := looksLikeQuotaExhaustion(cli, filler)
			refusalSealed := looksLikeQuotaExhaustion(cli, refused)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			if fillerSealed || !refusalSealed {
				t.Errorf("%s, %q filler: sealed=%t, with its refusal at the end sealed=%t", cli, unit, fillerSealed, refusalSealed)
			}
			if alloc := after.TotalAlloc - before.TotalAlloc; !raceEnabled && alloc > 32<<20 {
				t.Errorf("%s, %q filler: classifying 2 x 10 MiB allocated %d MiB", cli, unit, alloc>>20)
			}
			if elapsed > 3*raceSlowdown*time.Second {
				t.Errorf("%s, %q filler: classifying 2 x 10 MiB took %v", cli, unit, elapsed)
			}
		}
	}
	// The whole error path of one Claude run, its failure envelope included,
	// stays near what capturing and compacting the message costs anyway (9 MiB
	// of stderr: the capture keeps 10 MiB of both streams together). The
	// envelope's message is ASCII: compactProviderMessage's strings.Fields
	// takes a far costlier path for non-ASCII text, which is not classifying.
	p := NewClaudeCLI("unused")
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		// #nosec G204 G702 -- fixed selector in the trusted Go test executable.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestR4LargeStderrChild$")
		cmd.Stdin = strings.NewReader(`{"type":"result","is_error":true,"result":"You've hit your monthly spend limit."}`)
		cmd.Env = append(os.Environ(), "MAKEWAND_CLAUDE_ERROR_TEST_R4_STDERR_BYTES="+strconv.Itoa(size-1<<20), "GORACE=atexit_sleep_ms=0")
		return cmd
	}
	ctx, cancel := context.WithTimeout(context.Background(), raceSlowdown*time.Minute)
	defer cancel()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	_, _, err := p.chatReservedAttempt(ctx, "fixture", "fixture", "")
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if !r4Sealed(err) {
		t.Errorf("claude envelope refusal behind 9 MiB of stderr classified as %.200v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; !raceEnabled && alloc > 256<<20 {
		t.Errorf("claude error path on 9 MiB of stderr allocated %d MiB", alloc>>20)
	}
	if elapsed > 15*raceSlowdown*time.Second {
		t.Errorf("claude error path on 9 MiB of stderr took %v", elapsed)
	}
}

// TestR4LargeStderrChild is the Claude CLI of TestCLIQuotaClassificationCostIsBounded_R4:
// its stdin on stdout, MAKEWAND_CLAUDE_ERROR_TEST_R4_STDERR_BYTES of short
// lines on stderr, exit 1 (the sandbox passes MAKEWAND_CLAUDE_ERROR_TEST_* to
// test children).
func TestR4LargeStderrChild(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("MAKEWAND_CLAUDE_ERROR_TEST_R4_STDERR_BYTES"))
	if err != nil {
		return
	}
	stdout, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(stdout)
	_, _ = io.WriteString(os.Stderr, strings.Repeat("x\n", n/2))
	os.Exit(1)
}

// r4GlogHeader is a glog line of agy whose message continues on the lines
// that follow it.
const r4GlogHeader = "E1012 08:11:57.633062  188 client.go:224] request failed:\n"

// Only the tail of a long output is read, but what comes before it still
// decides: a fence opened before the tail hides the content it encloses, a
// transcript section that started before the tail keeps its content task
// content, a glog entry that runs into the tail stays a diagnostic, and a
// prompt echo that started before the tail is skipped whole.
func TestCLIQuotaTailKeepsWhatPrecedesItInMind_R4(t *testing.T) {
	filler := strings.Repeat("x\n", 300000)
	quotaRate := []string{"quota", "rate"}
	for _, tc := range []struct {
		name, cli, text, prompt string
		want                    bool
	}{
		{"fence opened before the tail", "claude", "```\n" + filler + "Rate limit exceeded\n```\ndone\n", "", false},
		{"fences closed before the tail", "claude", "```\nRate limit exceeded\n```\n" + filler + "Rate limit exceeded\n", "", true},
		{"fenced refusal after a closed fence", "claude", "```\nok\n```\n" + filler + "```\nRate limit exceeded\n```\n", "", false},
		{"exec section content", "codex", "OpenAI Codex v0.162.1 (research preview)\n--------\nexec\nbash -lc 'make test' in /repo\n exited 1 in 1.0s:\n" + filler + "Error: 429 Too Many Requests\nRate limit exceeded\n", "", false},
		{"own error event after section content", "codex", "OpenAI Codex v0.162.1 (research preview)\n--------\ncodex\n" + filler + "ERROR: You've hit your usage limit. Try again at 8:00 PM.\n", "", true},
		{"fatal report after the last header", "codex", "codex\n" + filler + "tokens used\n12\nError: You've hit your usage limit.\n", "", true},
		{"no transcript structure: fatal report", "codex", filler + "Error: You've hit your usage limit.\n", "", true},
		{"no transcript structure: marked line", "codex", filler + "Error: 429 Too Many Requests\n", "", true},
		{"banner, then a section longer than the tail", "codex", "OpenAI Codex v0.162.1 (research preview)\n--------\ncodex\n" + filler + "Error: 429 Too Many Requests\nRetrying.\n", "", false},
		{"glog entry running into the tail", "agy", r4GlogHeader + strings.Repeat("x\n", 10000) + "[ERROR] RESOURCE_EXHAUSTED\n", "", false},
		{"glog entry from far before the tail", "agy", r4GlogHeader + filler + "[ERROR] RESOURCE_EXHAUSTED\n", "", false},
		{"glog entry ended before the tail", "agy", r4GlogHeader + "retrying\n\n" + strings.Repeat("x\n", 10000) + "[ERROR] RESOURCE_EXHAUSTED\n", "", true},
		{"glog entry ended by a top-level line before the tail", "agy", r4GlogHeader + filler + "Error: request failed\n" + strings.Repeat("x\n", 10000) + "[ERROR] RESOURCE_EXHAUSTED\n", "", true},
		{"fatal report after a section that starts before the tail", "codex", "OpenAI Codex v0.162.1 (research preview)\n--------\ncodex\n" + strings.Repeat("x\n", 10000) + "Error: You've hit your usage limit.\n", "", true},
	} {
		got := len(findCLIMessages(tc.cli, quotaRate, cliStreams{stderr: tc.text, apart: true}, tc.prompt)) > 0
		if got != tc.want || looksLikeQuotaExhaustion(tc.cli, tc.text) != tc.want {
			t.Errorf("%s: %s sealed=%t, want %t", tc.name, tc.cli, got, tc.want)
		}
	}
	// A prompt echo longer than the tail: its header is far before the tail.
	prompt := "Review the log below.\n" + strings.Repeat("log line\n", 10000) +
		"ERROR: You've hit your usage limit. Try again at 8:00 PM.\n" + strings.Repeat("log line\n", 100) + "Fix the job."
	transcript := "OpenAI Codex v0.162.1 (research preview)\n--------\nworkdir: /repo\n--------\nuser\n" + prompt +
		"\ncodex\nLooking.\nERROR: stream disconnected before completion: error sending request\n"
	if found := findCLIMessages("codex", quotaRate, cliStreams{stderr: transcript, apart: true}, prompt); len(found) > 0 {
		t.Errorf("codex echo of a long prompt read as its own message: %q", found)
	}
	if found := findCLIMessages("codex", quotaRate, cliStreams{stderr: transcript, apart: true}, ""); len(found) == 0 {
		t.Error("without the prompt, the echoed event is codex's (health.py reads it so too)")
	}
}

// codex exec echoes the prompt into its transcript after a "user" header:
// that is task content, on the streaming path as on Chat's.
func TestCodexPromptEchoNeverSeals_R4(t *testing.T) {
	prompt := "Our nightly job log ends with:\nERROR: You've hit your usage limit. Try again at 8:00 PM.\nMake the job wait and retry instead of failing."
	stderr := "OpenAI Codex v0.46.0 (research preview)\n--------\nworkdir: /repo\nmodel: gpt-5-codex\n--------\nuser\n" + prompt +
		"\ncodex\nI'll look at the job script.\nERROR: stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)\n"
	script := testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "output", Stderr: stderr, ExitCode: 1})
	messages := []Message{{Role: "user", Content: prompt}}

	ch, err := NewCodexCLI(script).ChatStream(context.Background(), messages, "", 256)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var streamErr error
	for chunk := range ch {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	if streamErr == nil || ErrorKindOf(streamErr) == ErrorKindRateLimit {
		t.Errorf("codex stream: prompt echo classified as %v (kind %v)", streamErr, ErrorKindOf(streamErr))
	}
	if _, _, err := NewCodexCLI(script).Chat(context.Background(), messages, "", 256); err == nil || ErrorKindOf(err) == ErrorKindRateLimit {
		t.Errorf("codex chat: prompt echo classified as %v (kind %v)", err, ErrorKindOf(err))
	}
	// codex's own refusal after the echo still seals.
	own := testfixture.WriteCLI(t, "codex", testfixture.Spec{Mode: "output", Stderr: strings.Replace(stderr, "ERROR: stream disconnected", "ERROR: You've hit your usage limit. Try again at 9:00 PM.\nERROR: stream disconnected", 1), ExitCode: 1})
	ch, err = NewCodexCLI(own).ChatStream(context.Background(), messages, "", 256)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	streamErr = nil
	for chunk := range ch {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	if ErrorKindOf(streamErr) != ErrorKindRateLimit {
		t.Errorf("codex stream: its own refusal after the echo classified as %v", streamErr)
	}
}
