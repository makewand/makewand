package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/makewand/makewand/execution"
)

func TestClaudeCLIErrorMessage(t *testing.T) {
	for _, tt := range []struct {
		name, raw, want string
	}{
		{"quota result", `{"type":"result","is_error":true,"result":"You've hit your monthly spend limit"}`, "You've hit your monthly spend limit"},
		{"provider errors", `{"type":"result","is_error":true,"errors":["Provider unavailable","Try later"]}`, "Provider unavailable\nTry later"},
		{"subtype failure", `{"type":"result","subtype":"error_during_execution","errors":["Provider unavailable"]}`, "Provider unavailable"},
		{"explicit false", `{"type":"result","subtype":"error_during_execution","is_error":false,"result":"monthly spend limit"}`, ""},
		{"normal result", `{"type":"result","result":"monthly spend limit"}`, ""},
		{"normal assistant", `{"type":"assistant","is_error":true,"result":"monthly spend limit"}`, ""},
		{"plain output", "The phrase monthly spend limit is an example.", ""},
		{"malformed", `{"type":"result","is_error":true,"result":"monthly spend limit"`, ""},
		{"invalid error type", `{"type":"result","is_error":true,"errors":{"secret":"private"}}`, claudeCLIErrorFallback},
		{"oversized", `{"type":"result","is_error":true,"result":"` + strings.Repeat("x", claudeCLIErrorJSONLimit) + `"}`, claudeCLIErrorFallback},
		{"oversized envelope", `{"type":"result","is_error":true,"result":"` + strings.Repeat("x", claudeCLIErrorEnvelopeLimit) + `"}`, ""},
		{"metadata excluded", `{"type":"result","is_error":true,"result":"Provider unavailable","session_id":"private-session","account":{"email":"private-account"},"usage":{"secret":"private-usage"}}`, "Provider unavailable"},
		{"empty failure", `{"type":"result","is_error":true,"session_id":"private-session"}`, "Claude CLI reported an error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeCLIErrorMessage([]byte(tt.raw)); got != tt.want {
				t.Fatalf("failure message = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClaudeCLIErrorMessageRedactsAndBounds(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"type": "result", "is_error": true,
		"result": "\x1b[31mProvider failed\x1b[0m\x00\u202e Bearer private-bearer sk-private-key-123456 api_key='private-key' access_token=private-access refresh_token=private-refresh session_id=private-session account_ref=private-account private@example.test " + strings.Repeat("错", 1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := claudeCLIErrorMessage(raw)
	if len(got) > claudeCLIErrorMessageLimit || !utf8.ValidString(got) || !strings.HasPrefix(got, "Provider failed") {
		t.Fatalf("failure message was not bounded valid text: length=%d valid=%t", len(got), utf8.ValidString(got))
	}
	for _, forbidden := range []string{"private-", "private@", "\x1b", "\x00", "\u202e"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("failure message contains sensitive/control text %q", forbidden)
		}
	}
}

func TestClaudeCLIExecutionErrorOutputClassification(t *testing.T) {
	quota := []byte(`{"type":"result","is_error":true,"result":"You've hit your monthly spend limit"}`)
	provider := []byte(`{"type":"result","is_error":true,"errors":["Provider unavailable"]}`)
	for _, tt := range []struct {
		name       string
		stdout     []byte
		stderr     string
		ctxErr     error
		runErr     error
		kind       ErrorKind
		message    string
		statusCode int
	}{
		{"stdout quota", quota, "", nil, errors.New("exit status 1"), ErrorKindRateLimit, "You've hit your monthly spend limit", 429},
		{"stdout provider", provider, "", nil, errors.New("exit status 1"), ErrorKindProvider, "Provider unavailable", 0},
		{"stderr priority", provider, "Primary stderr failure", nil, errors.New("exit status 1"), ErrorKindProvider, "Primary stderr failure", 0},
		{"stdout quota supplements warning", quota, "CLI warning", nil, errors.New("exit status 1"), ErrorKindRateLimit, "CLI warning You've hit your monthly spend limit", 429},
		{"stderr quota priority", provider, "Monthly spend limit reached", nil, errors.New("exit status 1"), ErrorKindRateLimit, "Monthly spend limit reached", 429},
		{"bad JSON fallback", []byte(`{"is_error":true`), "", nil, errors.New("exit status 1"), ErrorKindProvider, "exit status 1", 0},
		{"normal JSON fallback", []byte(`{"type":"result","is_error":false,"result":"monthly spend limit"}`), "", nil, errors.New("exit status 1"), ErrorKindProvider, "exit status 1", 0},
		{"context cancellation priority", quota, "", context.Canceled, errors.New("exit status 1"), ErrorKindCanceled, "canceled after 1s", 0},
		{"context deadline priority", quota, "", context.DeadlineExceeded, errors.New("exit status 1"), ErrorKindTimeout, "timeout after 1s", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := NewClaudeCLI("unused")
			err := p.formatExecutionError(tt.stdout, tt.stderr, tt.runErr, tt.ctxErr, time.Second)
			var perr *ProviderError
			if !errors.As(err, &perr) || perr.Kind != tt.kind || perr.Message != tt.message || perr.StatusCode != tt.statusCode {
				t.Fatalf("error = %#v, want kind=%s message=%q status=%d", err, tt.kind, tt.message, tt.statusCode)
			}
			if tt.kind == ErrorKindRateLimit && !perr.Retryable {
				t.Fatal("quota rejection must remain retryable")
			}
		})
	}
	p := NewClaudeCLI("unused")
	if err := p.formatExecutionError(quota, "", exec.ErrWaitDelay, nil, time.Second); !errors.Is(err, execution.ErrUnknownOutcome) {
		t.Fatalf("incomplete process output lost UNKNOWN priority: %v", err)
	}
	custom := NewCommandCLI("claude", "unused", nil, PromptModeArg)
	if err := custom.formatExecutionError(quota, "", errors.New("exit status 1"), nil, time.Second); ErrorKindOf(err) != ErrorKindProvider || strings.Contains(err.Error(), "monthly spend limit") {
		t.Fatalf("custom provider interpreted a Claude-specific failure envelope: %v", err)
	}
}

func TestClaudeCLIErrorOutputChild(t *testing.T) {
	if os.Getenv("MAKEWAND_CLAUDE_ERROR_TEST_CHILD") != "1" {
		return
	}
	stdout, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(stdout)
	fmt.Fprint(os.Stderr, os.Getenv("MAKEWAND_CLAUDE_ERROR_TEST_STDERR"))
	if os.Getenv("MAKEWAND_CLAUDE_ERROR_TEST_SUCCESS") == "1" {
		os.Exit(0)
	}
	os.Exit(1)
}

func TestClaudeCLIErrorOutputFromProcess(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, successfulExit := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/exitZero=%t", streaming, successfulExit), func(t *testing.T) {
				stdout := `{"type":"result","is_error":true,"errors":["You've hit your monthly spend limit"],"session_id":"private-session","account":"private-account","usage":{"secret":"private-usage"}}` + "\n"
				p := NewClaudeCLI("unused")
				p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
					// #nosec G204 G702 -- fixed selector in the trusted Go test executable.
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaudeCLIErrorOutputChild$")
					cmd.Stdin = strings.NewReader(stdout)
					cmd.Env = append(os.Environ(), "MAKEWAND_CLAUDE_ERROR_TEST_CHILD=1", "MAKEWAND_CLAUDE_ERROR_TEST_STDERR=", "MAKEWAND_CLAUDE_ERROR_TEST_SUCCESS=0", "GORACE=atexit_sleep_ms=0")
					if successfulExit {
						cmd.Env = append(cmd.Env, "MAKEWAND_CLAUDE_ERROR_TEST_SUCCESS=1")
					}
					return cmd
				}
				p.buildStreamCmd = p.buildCmd
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var err error
				var content string
				if streaming {
					content, err = collectCLIProcessStream(ctx, p)
				} else {
					content, _, err = p.chatReservedAttempt(ctx, "fixture", "fixture", "")
				}
				if content != "" {
					t.Fatalf("failure envelope leaked into answer content: %q", content)
				}
				var perr *ProviderError
				if !errors.As(err, &perr) || perr.Kind != ErrorKindRateLimit || perr.StatusCode != 429 || !perr.Retryable {
					t.Fatalf("stdout quota error was not classified: %v", err)
				}
				if !strings.Contains(err.Error(), "monthly spend limit") || strings.Contains(err.Error(), "private-") {
					t.Fatalf("stdout failure diagnostic lost the limit or leaked metadata: %v", err)
				}
			})
		}
	}
}

func TestClaudeCLIStreamErrorMetadataCannotEscapeMalformedDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		name, stdout string
	}{
		{"oversized", `{"type":"result","is_error":true,"result":"` + strings.Repeat("x", claudeCLIErrorJSONLimit) + `","session_id":"private-session","account":"private-account"}`},
		{"wrong error type", `{"type":"result","is_error":true,"errors":{"secret":"private-secret"},"session_id":"private-session","account":"private-account"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := NewClaudeCLI("unused")
			p.buildStreamCmd = func(ctx context.Context, prompt string) *exec.Cmd {
				// #nosec G204 G702 -- fixed selector in the trusted Go test executable.
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaudeCLIErrorOutputChild$")
				cmd.Stdin = strings.NewReader(tt.stdout + "\n")
				cmd.Env = append(os.Environ(), "MAKEWAND_CLAUDE_ERROR_TEST_CHILD=1", "MAKEWAND_CLAUDE_ERROR_TEST_STDERR=", "MAKEWAND_CLAUDE_ERROR_TEST_SUCCESS=0", "GORACE=atexit_sleep_ms=0")
				return cmd
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			content, err := collectCLIProcessStream(ctx, p)
			if content != "" || err == nil || !strings.Contains(err.Error(), claudeCLIErrorFallback) || strings.Contains(err.Error(), "private-") {
				t.Fatalf("unsafe stream failure diagnostic: content_bytes=%d err=%v", len(content), err)
			}
		})
	}
}

func TestClaudeCLIErrorOutputFromLargeSuccessfulProcess(t *testing.T) {
	stdout := `{"type":"result","is_error":true,"result":"` + strings.Repeat("x", (1<<20)+1) + `","session_id":"private-session","account":"private-account"}`
	p := NewClaudeCLI("unused")
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		// #nosec G204 G702 -- fixed selector in the trusted Go test executable.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaudeCLIErrorOutputChild$")
		cmd.Stdin = strings.NewReader(stdout)
		cmd.Env = append(os.Environ(), "MAKEWAND_CLAUDE_ERROR_TEST_CHILD=1", "MAKEWAND_CLAUDE_ERROR_TEST_STDERR=", "MAKEWAND_CLAUDE_ERROR_TEST_SUCCESS=1", "GORACE=atexit_sleep_ms=0")
		return cmd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	content, _, err := p.chatReservedAttempt(ctx, "fixture", "fixture", "")
	if content != "" || err == nil || !strings.Contains(err.Error(), claudeCLIErrorFallback) || strings.Contains(err.Error(), "private-") {
		t.Fatalf("large failed envelope became an answer: content_bytes=%d err=%v", len(content), err)
	}
}
