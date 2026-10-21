package router

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/testfixture"
)

// GR-1: an ordinary CLI exit is no longer UNKNOWN across the board. The CLI's
// own refusal of the invocation (login, arguments) is a known FAILED that
// falls back; an unrecognized exit stays UNKNOWN, and stops replay only when
// the run could have written the workspace.

// isolateCLITestHome keeps a fixture CLI named after a real provider away from
// the developer's credentials (~/.grok and the like).
func isolateCLITestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// exitingCLI is a custom CLI provider named name whose fixture prints stderr
// and exits 1 without doing any model work.
func exitingCLI(t *testing.T, name, stderr string) *CLIProvider {
	t.Helper()
	script := testfixture.WriteCLI(t, name, testfixture.Spec{Stderr: stderr, ExitCode: 1})
	return NewCommandCLI(name, script, nil, PromptModeStdin)
}

// fallbackRouter routes every task to primary, with a healthy gemini behind it.
func fallbackRouter(primary *CLIProvider, fallback Provider) *Router {
	name := primary.Name()
	return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
		name:     {Provider: primary, Access: AccessSubscription},
		"gemini": {Provider: fallback, Access: AccessSubscription},
	}, DefaultModel: name, CodingModel: name, ReviewModel: name})
}

// chatOrStream runs one routed request and returns its whole output.
func chatOrStream(ctx context.Context, r *Router, task TaskType, streaming bool) (string, RouteResult, error) {
	messages := []Message{{Role: "user", Content: "do the task"}}
	if !streaming {
		out, _, route, err := r.Chat(ctx, task, messages, "")
		return out, route, err
	}
	stream, route, err := r.ChatStream(ctx, task, messages, "")
	if err != nil {
		return "", route, err
	}
	var out strings.Builder
	for chunk := range stream {
		if chunk.Error != nil {
			return out.String(), route, chunk.Error
		}
		out.WriteString(chunk.Content)
	}
	return out.String(), route, nil
}

func TestCLIPreDispatchRefusalFallsBackToHealthyProvider(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		isolateCLITestHome(t)
		ctx, ledger := budgetTestContext(t, 5)
		// A writable run: a refusal before any model work cannot have edited it.
		ctx = ContextWithWorkDir(ctx, t.TempDir())
		var fallbackCalls atomic.Int32
		r := fallbackRouter(exitingCLI(t, "grok", "Error: Not logged in. Run `grok login`.\n"), &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls})
		out, route, err := chatOrStream(ctx, r, TaskCode, streaming)
		if err != nil || route.Actual != "gemini" || fallbackCalls.Load() != 1 || !strings.HasPrefix(out, "synthetic") {
			t.Fatalf("stream=%v: out=%q route=%+v err=%v fallback calls=%d", streaming, out, route, err, fallbackCalls.Load())
		}
		attempts := budgetTestAttempts(t, ledger)
		if len(attempts) != 2 || attempts[0]["engine"] != "grok" || attempts[0]["result_status"] != "FAILED" || attempts[0]["outcome_known"] != true || attempts[1]["result_status"] != "PASSED" {
			t.Fatalf("stream=%v: refusal not recorded as a known failure: %+v", streaming, attempts)
		}
	}
}

// The reviewer's probe: ChatWith pinned to a CLI that is not logged in, with a
// healthy provider registered behind it.
func TestCLIPreDispatchRefusalFallsBackInChatWith(t *testing.T) {
	isolateCLITestHome(t)
	ctx := ContextWithWorkDir(context.Background(), t.TempDir())
	var fallbackCalls atomic.Int32
	r := fallbackRouter(exitingCLI(t, "grok", "Error: Not logged in. Run `grok login`.\n"), &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls})
	out, _, route, err := r.ChatWith(ctx, "grok", PhaseCode, []Message{{Role: "user", Content: "implement it"}}, "")
	if err != nil || out != "synthetic output" || route.Actual != "gemini" || fallbackCalls.Load() != 1 {
		t.Fatalf("out=%q route=%+v err=%v fallback calls=%d", out, route, err, fallbackCalls.Load())
	}
}

func TestCLIReadOnlyUnknownExitFallsBack(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		isolateCLITestHome(t)
		ctx, ledger := budgetTestContext(t, 5)
		ctx = ContextWithWorkDir(ctx, t.TempDir())
		var fallbackCalls atomic.Int32
		r := fallbackRouter(exitingCLI(t, "grok", "Error: internal failure in turn 3\n"), &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls})
		out, route, err := chatOrStream(ctx, r, TaskReview, streaming)
		if err != nil || route.Actual != "gemini" || fallbackCalls.Load() != 1 || !strings.HasPrefix(out, "synthetic") {
			t.Fatalf("stream=%v: read-only unknown exit stopped fallback: out=%q route=%+v err=%v fallback calls=%d", streaming, out, route, err, fallbackCalls.Load())
		}
		// The budget keeps the conservative charge: the outcome stays unknown.
		attempts := budgetTestAttempts(t, ledger)
		if len(attempts) != 2 || attempts[0]["engine"] != "grok" || attempts[0]["result_status"] != "UNKNOWN" || attempts[0]["outcome_known"] != false || attempts[1]["result_status"] != "PASSED" {
			t.Fatalf("stream=%v: read-only unknown exit accounting: %+v", streaming, attempts)
		}
	}
}

func TestCLIWritableUnknownExitStopsFallback(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		isolateCLITestHome(t)
		ctx, ledger := budgetTestContext(t, 5)
		ctx = ContextWithWorkDir(ctx, t.TempDir())
		var fallbackCalls atomic.Int32
		r := fallbackRouter(exitingCLI(t, "grok", "Error: internal failure in turn 3\n"), &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls})
		_, _, err := chatOrStream(ctx, r, TaskCode, streaming)
		if !errors.Is(err, execution.ErrUnknownOutcome) || fallbackCalls.Load() != 0 {
			t.Fatalf("stream=%v: writable unknown exit replayed: err=%v fallback calls=%d", streaming, err, fallbackCalls.Load())
		}
		attempts := budgetTestAttempts(t, ledger)
		if len(attempts) != 1 || attempts[0]["result_status"] != "UNKNOWN" || attempts[0]["outcome_known"] != false {
			t.Fatalf("stream=%v: writable unknown exit accounting: %+v", streaming, attempts)
		}
	}
}

// The provider's own refusals, as health.py's adapters read them: catalog
// "auth" messages and agy's argument rejections (its first own line).
func TestCLIInvocationRefusalClassification(t *testing.T) {
	const keyringOK = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
	const startup = "E1012 08:11:57.633062     188 errorreport.go:224] error getting token source: You are not logged into Antigravity.\n" +
		"I1012 08:11:57.843790       1 printmode.go:403] Print mode: not authenticated, trying silent auth\n"
	const modeWarning = "warning: --mode plan is not supported in print mode; continuing in the default mode.\n"
	const envWarning = "Warning: Both GOOGLE_API_KEY and GEMINI_API_KEY are set. Using GOOGLE_API_KEY.\n"
	const rejection = `error: invalid model selection (--model "gemini-3.1-pro" --effort "max"): gemini-3.1-pro has no "max" effort (available: low, high)` + "\n"
	exit := errors.New("exit status 1")
	grok := NewCommandCLI("grok", "unused", nil, PromptModeArg)
	muse := NewCommandCLI("muse", "unused", nil, PromptModeArg)
	agy := NewAgyCLI("unused")
	for _, tt := range []struct {
		name   string
		cli    *CLIProvider
		stderr string
		want   ErrorKind
	}{
		{"grok login", grok, "Error: Not logged in. Run `grok login`.\n", ErrorKindAuth},
		{"grok expired session", grok, "Your session has expired. Run `grok login` to sign in again.\n", ErrorKindAuth},
		{"muse login", muse, "not logged in: run /login to add an API key\n", ErrorKindAuth},
		{"agy login", agy, "Error: You are not logged into Antigravity. Run `agy` to sign in.\n", ErrorKindAuth},
		{"agy backend rejection after keyring", agy, keyringOK + "Error: UNAUTHENTICATED: Request had invalid authentication credentials.\n", ErrorKindAuth},
		{"agy model rejection", agy, "\n" + keyringOK + rejection, ErrorKindConfig},
		{"agy rejection after start-up notices", agy, startup + keyringOK + rejection, ErrorKindConfig},
		{"agy rejection after mode warning", agy, keyringOK + modeWarning + rejection, ErrorKindConfig},
		{"agy rejection after env warning", agy, envWarning + keyringOK + rejection, ErrorKindConfig},
		{"agy unknown flag", agy, "\nError: unknown flag: --effort\nUsage:\n  agy [flags]\n", ErrorKindConfig},
		{"agy unknown flag CRLF", agy, "Error: unknown flag: --effort\r\nUsage:\r\n  agy [flags]\r\n", ErrorKindConfig},
		{"agy unknown flag colored", agy, "\x1b[31mError:\x1b[0m unknown flag: --effort\n", ErrorKindConfig},
		// The same words as task content, or a moot notice, are no refusal.
		{"tool output", agy, "Ran `tar -xzf a.tgz --strip`: tar: unknown option -- strip\nPartial changes written.\n", ErrorKindProvider},
		{"later line", agy, "I edited cli.py.\nerror: unknown flag: --json (from the gh call)\n", ErrorKindProvider},
		{"answer opening", agy, "Error: unknown flag: --json -- your gh is too old, so I rewrote scripts/ci.sh without it.\nPartial changes written.\n", ErrorKindProvider},
		{"moot start-up notice", agy, keyringOK + "You are not logged into Antigravity.\n", ErrorKindProvider},
		{"docs quote", agy, keyringOK + "Docs: users see 'You are not logged into Antigravity' until they sign in.\n", ErrorKindProvider},
		{"grok task failure", grok, "Error: internal failure in turn 3\n", ErrorKindProvider},
		// No login kind in the catalog: these CLIs' exits stay unrecognized.
		{"claude", NewClaudeCLI("unused"), "Not logged in · Please run /login\n", ErrorKindProvider},
		{"codex", NewCodexCLI("unused"), "Error: Not logged in\n", ErrorKindProvider},
		{"custom", NewCommandCLI("offline", "unused", nil, PromptModeArg), "Error: not logged in\n", ErrorKindProvider},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cli.formatExecutionError(nil, tt.stderr, "", exit, nil, time.Second)
			var perr *ProviderError
			if !errors.As(err, &perr) || perr.Kind != tt.want || perr.Op != "CLI" || perr.Retryable {
				t.Fatalf("error = %#v, want a non-retryable CLI error of kind %s", err, tt.want)
			}
			if outcome := providerOutcome(err, Usage{}); outcome.Known != (tt.want != ErrorKindProvider) {
				t.Fatalf("outcome %+v: known=%v, want %v", outcome, outcome.Known, tt.want != ErrorKindProvider)
			}
			if tt.want == ErrorKindConfig && strings.Contains(perr.Message, "I1012") {
				t.Fatalf("glog diagnostics leaked into the refusal: %q", perr.Message)
			}
		})
	}
}

// A refusal never overrides a lost exit result or an account switch.
func TestCLIInvocationRefusalKeepsUnknownEvidence(t *testing.T) {
	grok := NewCommandCLI("grok", "unused", nil, PromptModeArg)
	login := "Error: Not logged in. Run `grok login`.\n"
	switched := errors.Join(errors.New("exit status 1"), &execution.UnknownOutcomeError{Err: errors.New("selected login changed during direct execution")})
	for _, runErr := range []error{exec.ErrWaitDelay, switched} {
		if err := grok.formatExecutionError(nil, login, "", runErr, nil, time.Second); !errors.Is(err, execution.ErrUnknownOutcome) {
			t.Fatalf("run error %v: refusal hid an unknown outcome: %v", runErr, err)
		}
	}
	if err := grok.formatExecutionError(nil, login, "", errors.New("exit status 1"), context.DeadlineExceeded, time.Second); ErrorKindOf(err) != ErrorKindTimeout {
		t.Fatalf("deadline lost its priority: %v", err)
	}
}

// Power: one generator ending UNKNOWN does not discard another's answer; the
// unknown attempt stays in the trace and the usage. If every generator fails
// the unknown outcome is still returned (TestPowerUnknownGenerationIsNotReplayedThroughAdaptiveFallback).
func TestChatBestPowerUnknownGeneratorReturnsOtherGeneratorsAnswer(t *testing.T) {
	isolateCLITestHome(t)
	failed := &spendThenFailProvider{
		ensembleCaptureProvider: ensembleCaptureProvider{name: "claude", available: true, usage: measuredUsage(1, 10, 1)},
		err:                     newProviderError("claude", "CLI", ErrorKindProvider, false, 0, "claude CLI exited 1", nil),
	}
	ok := &ensembleCaptureProvider{name: "codex", available: true, content: "codex answer", usage: measuredUsage(2, 20, 2)}
	judge := &ensembleCaptureProvider{name: "gemini", available: false}
	r := powerReviewRouter(judge, failed, ok)
	var mu sync.Mutex
	var events []TraceEvent
	r.SetTraceSink(TraceSinkFunc(func(ev TraceEvent) { mu.Lock(); events = append(events, ev); mu.Unlock() }))

	out, total, route, err := r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "implement it"}}, "")
	if err != nil || out != "codex answer" || route.Actual != "codex" {
		t.Fatalf("ChatBest = %q route=%+v err=%v, want the surviving codex answer", out, route, err)
	}
	if total.Cost != 3 || total.InputTokens != 30 || total.OutputTokens != 3 {
		t.Fatalf("usage %+v lost the unknown attempt", total)
	}
	unknownTraced := false
	for _, ev := range events {
		if ev.Event == "ensemble_generator_error" && ev.Selected == "claude" && strings.Contains(ev.Error, "outcome unknown") {
			unknownTraced = true
		}
	}
	if !unknownTraced {
		t.Fatalf("unknown generator attempt missing from the trace: %+v", events)
	}
}

// A judge ending UNKNOWN cannot pick a winner, but the candidates it was
// asked about are kept: ChatBest returns the first one, as for a failed judge.
func TestChatBestPowerUnknownJudgeReturnsFirstCandidate(t *testing.T) {
	isolateCLITestHome(t)
	first := &ensembleCaptureProvider{name: "claude", available: true, content: "claude answer", usage: measuredUsage(1, 10, 1)}
	second := &ensembleCaptureProvider{name: "codex", available: true, content: "codex answer", usage: measuredUsage(2, 20, 2)}
	judge := &spendThenFailProvider{
		ensembleCaptureProvider: ensembleCaptureProvider{name: "gemini", available: true, usage: measuredUsage(4, 40, 4)},
		err:                     newProviderError("gemini", "CLI", ErrorKindProvider, false, 0, "judge CLI exited 1", nil),
	}
	r := powerReviewRouter(judge, first, second)
	out, total, _, err := r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "implement it"}}, "")
	if err != nil || (out != "claude answer" && out != "codex answer") {
		t.Fatalf("ChatBest = %q err=%v, want a generator's answer", out, err)
	}
	if total.Cost != 7 || total.InputTokens != 70 || total.OutputTokens != 7 {
		t.Fatalf("usage %+v lost an attempt", total)
	}
}

// With fallback restored, a CLI that refuses before dispatch costs its half-open
// probe nothing but a fast refusal: the probing request is served by the
// fallback, and every failed probe doubles the next cooldown.
func TestCLIRefusalHalfOpenProbeFallsBackAndBacksOff(t *testing.T) {
	isolateCLITestHome(t)
	ctx := ContextWithWorkDir(context.Background(), t.TempDir())
	var fallbackCalls atomic.Int32
	r := fallbackRouter(exitingCLI(t, "grok", "Error: Not logged in. Run `grok login`.\n"), &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls})
	now := time.Now()
	r.breaker.now = func() time.Time { return now }
	for i := 1; i <= defaultCircuitFailureThreshold; i++ {
		if _, route, err := chatOrStream(ctx, r, TaskCode, false); err != nil || route.Actual != "gemini" {
			t.Fatalf("request %d: route=%+v err=%v", i, route, err)
		}
	}
	open, remaining := r.breaker.PeekOpen("grok")
	if !open || remaining != defaultCircuitCooldown {
		t.Fatalf("refusals did not open the circuit: open=%v remaining=%v", open, remaining)
	}
	for probe, cooldown := range []time.Duration{defaultCircuitCooldown, 2 * defaultCircuitCooldown, 4 * defaultCircuitCooldown} {
		now = now.Add(remaining)
		calls := fallbackCalls.Load()
		if _, route, err := chatOrStream(ctx, r, TaskCode, false); err != nil || route.Actual != "gemini" || fallbackCalls.Load() != calls+1 {
			t.Fatalf("probe %d lost its request: route=%+v err=%v", probe, route, err)
		}
		if open, remaining = r.breaker.PeekOpen("grok"); !open || remaining != cooldown {
			t.Fatalf("probe %d: open=%v remaining=%v, want %v", probe, open, remaining, cooldown)
		}
	}
}
