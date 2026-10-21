package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// the developer's credentials (~/.grok and the like) and returns its HOME.
func isolateCLITestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// exitingCLI is a custom CLI provider named name whose fixture prints stderr
// and exits 1 without doing any model work.
func exitingCLI(t *testing.T, name, stderr string) *CLIProvider {
	t.Helper()
	script := testfixture.WriteCLI(t, name, testfixture.Spec{Stderr: stderr, ExitCode: 1})
	return NewCommandCLI(name, script, nil, PromptModeStdin)
}

// failingCLI is a CLI provider whose fixture prints spec's stdout and stderr
// and exits 1: the router's agy transport (its "gemini") for name "agy",
// otherwise a custom command named name. For agy, home (isolateCLITestHome)
// gets the settings proving G1 credits off, which a launch outside the
// sandbox requires, and with them the .gemini directory on which the
// sandbox of a read-only run mounts its own.
func failingCLI(t *testing.T, home, name string, spec testfixture.Spec) *CLIProvider {
	t.Helper()
	spec.ExitCode = 1
	script := testfixture.WriteCLI(t, name, spec)
	if name != "agy" {
		return NewCommandCLI(name, script, nil, PromptModeStdin)
	}
	off := `{"useG1Credits": false}`
	agyWriteHostSettings(t, home, [2]string{off, off})
	agy := NewAgyCLI(script)
	agy.cachedAvail, agy.cachedAvailAt = true, time.Now() // never probe `agy models`
	return agy
}

// fallbackRouter routes every task to primary, with fallback behind it.
func fallbackRouter(primary *CLIProvider, fallback Provider) *Router {
	name := primary.Name()
	return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{
		name:            {Provider: primary, Access: AccessSubscription},
		fallback.Name(): {Provider: fallback, Access: AccessSubscription},
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
	// A review is read-only on the sandbox's read-only mount of its workspace.
	auditFixRequireLiveBwrap(t)
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

// A login refusal after the model answered, or after other lines of the run,
// is in the words of one at the start, but the run may have edited the
// workspace: a writable run stays unknown and is never replayed, while the
// same output in a read-only run still falls back.
func TestCLILoginLostMidRunStopsWritableReplay(t *testing.T) {
	for _, tt := range []struct {
		name, cli string
		spec      testfixture.Spec
	}{
		{"agy answered, then unauthenticated", "agy", testfixture.Spec{
			Stdout: "Updated router/cli.go and router/cli_test.go; running the tests next.\n",
			Stderr: "Error: rpc error: code = Unauthenticated desc = Request had invalid authentication credentials.\n",
		}},
		{"grok answered, then its session expired", "grok", testfixture.Spec{
			Stdout: "Edited the handler; tests next.\n",
			Stderr: "Your session has expired. Run `grok login` to sign in again.\n",
		}},
		{"grok session expired after its progress", "grok", testfixture.Spec{
			Stderr: "Edited router/cli.go\nYour session has expired. Run `grok login` to sign in again.\n",
		}},
	} {
		t.Run(tt.name+"/writable", func(t *testing.T) {
			for _, streaming := range []bool{false, true} {
				home := isolateCLITestHome(t)
				ctx, ledger := budgetTestContext(t, 5)
				ctx = ContextWithWorkDir(ctx, t.TempDir())
				var fallbackCalls atomic.Int32
				r := fallbackRouter(failingCLI(t, home, tt.cli, tt.spec), &dispatchBudgetProvider{name: "codex", calls: &fallbackCalls})
				_, route, err := chatOrStream(ctx, r, TaskCode, streaming)
				if !errors.Is(err, execution.ErrUnknownOutcome) || fallbackCalls.Load() != 0 {
					t.Fatalf("stream=%v: writable run replayed: route=%+v err=%v fallback calls=%d", streaming, route, err, fallbackCalls.Load())
				}
				attempts := budgetTestAttempts(t, ledger)
				if len(attempts) != 1 || attempts[0]["result_status"] != "UNKNOWN" || attempts[0]["outcome_known"] != false {
					t.Fatalf("stream=%v: writable run accounting: %+v", streaming, attempts)
				}
			}
		})
		t.Run(tt.name+"/read-only", func(t *testing.T) {
			// A review runs on a read-only mount: nothing can stack on it.
			auditFixRequireLiveBwrap(t)
			home := isolateCLITestHome(t)
			ctx, ledger := budgetTestContext(t, 5)
			ctx = ContextWithWorkDir(ctx, t.TempDir())
			var fallbackCalls atomic.Int32
			primary := failingCLI(t, home, tt.cli, tt.spec)
			r := fallbackRouter(primary, &dispatchBudgetProvider{name: "codex", calls: &fallbackCalls})
			out, route, err := chatOrStream(ctx, r, TaskReview, false)
			if err != nil || route.Actual != "codex" || out != "synthetic output" || fallbackCalls.Load() != 1 {
				t.Fatalf("read-only run did not fall back: out=%q route=%+v err=%v fallback calls=%d", out, route, err, fallbackCalls.Load())
			}
			attempts := budgetTestAttempts(t, ledger)
			if len(attempts) != 2 || attempts[0]["engine"] != primary.Name() || attempts[0]["result_status"] != "FAILED" || attempts[0]["outcome_known"] != true {
				t.Fatalf("read-only login refusal accounting: %+v", attempts)
			}
		})
	}
}

// A run is read-only only where it cannot have written the workspace. A
// remote-origin request with a WorkDir of its own has it mounted writable,
// and a review outside the sandbox (acknowledged host execution) has no
// read-only mount: their unrecognized exits stop replay like any writable
// run's. A remote-origin request without a WorkDir runs in a scratch
// directory and falls back.
func TestCLIReadOnlyRunNeedsAReadOnlyWorkspace(t *testing.T) {
	// run returns the fallback calls, the grok attempt's ledger status ("" when
	// the fixture never ran) and the error of one request in ctx.
	run := func(t *testing.T, ctx context.Context, task TaskType) (int32, any, error) {
		t.Helper()
		isolateCLITestHome(t)
		ledger := filepath.Join(t.TempDir(), "ledger.json")
		ctx = execution.ContextWithConfig(ctx, execution.Config{LedgerPath: ledger, MaxCalls: 5, TaskID: "router-read-only-test"})
		var fallbackCalls atomic.Int32
		r := fallbackRouter(exitingCLI(t, "grok", "Error: internal failure in turn 3\n"), &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls})
		_, _, err := chatOrStream(ctx, r, task, false)
		var status any = ""
		if attempts := budgetTestAttempts(t, ledger); len(attempts) > 0 && attempts[0]["engine"] == "grok" {
			status = attempts[0]["result_status"]
		}
		return fallbackCalls.Load(), status, err
	}
	t.Run("review outside the sandbox", func(t *testing.T) {
		old := cliBwrapLookup
		cliBwrapLookup = func(string) (string, error) { return "", exec.ErrNotFound }
		t.Cleanup(func() { cliBwrapLookup = old })
		t.Setenv("MAKEWAND_CONFIG_DIR", os.Getenv("MAKEWAND_CONFIG_DIR"))
		t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC"))
		ack, err := cliTestWriteHostExecAck()
		t.Cleanup(func() { _ = os.RemoveAll(ack) })
		if err != nil {
			t.Fatal(err)
		}
		fallbackCalls, status, err := run(t, ContextWithWorkDir(context.Background(), t.TempDir()), TaskReview)
		if !errors.Is(err, execution.ErrUnknownOutcome) || fallbackCalls != 0 || status != "UNKNOWN" {
			t.Fatalf("unsandboxed review: err=%v fallback calls=%d grok status=%v, want an unknown outcome that stops replay", err, fallbackCalls, status)
		}
	})
	t.Run("remote origin with a WorkDir", func(t *testing.T) {
		auditFixRequireLiveBwrap(t)
		fallbackCalls, status, err := run(t, ContextWithWorkDir(ContextWithRemoteOrigin(context.Background()), t.TempDir()), TaskCode)
		if !errors.Is(err, execution.ErrUnknownOutcome) || fallbackCalls != 0 || status != "UNKNOWN" {
			t.Fatalf("remote run with a writable WorkDir: err=%v fallback calls=%d grok status=%v, want an unknown outcome that stops replay", err, fallbackCalls, status)
		}
	})
	t.Run("remote origin in a scratch directory", func(t *testing.T) {
		auditFixRequireLiveBwrap(t)
		fallbackCalls, status, err := run(t, ContextWithRemoteOrigin(context.Background()), TaskCode)
		if err != nil || fallbackCalls != 1 || status != "UNKNOWN" {
			t.Fatalf("remote run in a scratch directory: err=%v fallback calls=%d grok status=%v, want an unknown outcome that falls back", err, fallbackCalls, status)
		}
	})
}

// agy rejecting its arguments is a known FAILED to callers too (exit 10, as
// health.py reports it), not an invalid request.
func TestCLIArgumentRejectionEndsFailed(t *testing.T) {
	home := isolateCLITestHome(t)
	ctx := ContextWithWorkDir(context.Background(), t.TempDir())
	var fallbackCalls atomic.Int32
	denied := newProviderError("codex", "chat", ErrorKindAuth, false, 401, "denied", nil)
	r := fallbackRouter(failingCLI(t, home, "agy", testfixture.Spec{Stderr: "Error: unknown flag: --effort\n"}), &dispatchBudgetProvider{name: "codex", calls: &fallbackCalls, err: denied})
	_, _, err := chatOrStream(ctx, r, TaskCode, false)
	if fallbackCalls.Load() != 1 || ErrorKindOf(err) != ErrorKindConfig || execution.ErrorStatus(err) != execution.Failed {
		t.Fatalf("err=%v kind=%s status=%s fallback calls=%d, want agy's rejection as FAILED after the fallback", err, ErrorKindOf(err), execution.ErrorStatus(err), fallbackCalls.Load())
	}
}

// The provider's own refusals, as health.py's adapters read them: catalog
// "auth" messages and agy's argument rejections (its first own line). A run
// that could have written the workspace counts one only when it is all the
// run shows: no answer on stdout, and the refusal is the CLI's first own line.
func TestCLIInvocationRefusalClassification(t *testing.T) {
	const keyringOK = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
	const startup = "E1012 08:11:57.633062     188 errorreport.go:224] error getting token source: You are not logged into Antigravity.\n" +
		"I1012 08:11:57.843790       1 printmode.go:403] Print mode: not authenticated, trying silent auth\n"
	const modeWarning = "warning: --mode plan is not supported in print mode; continuing in the default mode.\n"
	const envWarning = "Warning: Both GOOGLE_API_KEY and GEMINI_API_KEY are set. Using GOOGLE_API_KEY.\n"
	const rejection = `error: invalid model selection (--model "gemini-3.1-pro" --effort "max"): gemini-3.1-pro has no "max" effort (available: low, high)` + "\n"
	const agyUnauthenticated = "Error: rpc error: code = Unauthenticated desc = Request had invalid authentication credentials.\n"
	const grokExpired = "Your session has expired. Run `grok login` to sign in again.\n"
	const answer = "Updated router/cli.go and router/cli_test.go; running the tests next.\n"
	exit := errors.New("exit status 1")
	grok := NewCommandCLI("grok", "unused", nil, PromptModeArg)
	muse := NewCommandCLI("muse", "unused", nil, PromptModeArg)
	agy := NewAgyCLI("unused")
	for _, tt := range []struct {
		name             string
		cli              *CLIProvider
		stdout, stderr   string
		readOnly, writes ErrorKind // in a read-only run, in one that could have written
	}{
		{"grok login", grok, "", "Error: Not logged in. Run `grok login`.\n", ErrorKindAuth, ErrorKindAuth},
		{"grok expired session", grok, "", grokExpired, ErrorKindAuth, ErrorKindAuth},
		{"muse login", muse, "", "not logged in: run /login to add an API key\n", ErrorKindAuth, ErrorKindAuth},
		{"agy login", agy, "", "Error: You are not logged into Antigravity. Run `agy` to sign in.\n", ErrorKindAuth, ErrorKindAuth},
		{"agy login after start-up notices", agy, "", startup + "Error: Authentication required. Please visit the URL to log in\n", ErrorKindAuth, ErrorKindAuth},
		{"agy backend rejection after keyring", agy, "", keyringOK + "Error: UNAUTHENTICATED: Request had invalid authentication credentials.\n", ErrorKindAuth, ErrorKindAuth},
		{"agy login after a notice", agy, "", envWarning + agyUnauthenticated, ErrorKindAuth, ErrorKindAuth},
		{"login beside a blank answer", grok, "\n \x1b[0m\n", "Error: Not logged in. Run `grok login`.\n", ErrorKindAuth, ErrorKindAuth},
		{"agy model rejection", agy, "", "\n" + keyringOK + rejection, ErrorKindConfig, ErrorKindConfig},
		{"agy rejection after start-up notices", agy, "", startup + keyringOK + rejection, ErrorKindConfig, ErrorKindConfig},
		{"agy rejection after mode warning", agy, "", keyringOK + modeWarning + rejection, ErrorKindConfig, ErrorKindConfig},
		{"agy rejection after env warning", agy, "", envWarning + keyringOK + rejection, ErrorKindConfig, ErrorKindConfig},
		{"agy unknown flag", agy, "", "\nError: unknown flag: --effort\nUsage:\n  agy [flags]\n", ErrorKindConfig, ErrorKindConfig},
		{"agy unknown flag CRLF", agy, "", "Error: unknown flag: --effort\r\nUsage:\r\n  agy [flags]\r\n", ErrorKindConfig, ErrorKindConfig},
		{"agy unknown flag colored", agy, "", "\x1b[31mError:\x1b[0m unknown flag: --effort\n", ErrorKindConfig, ErrorKindConfig},
		// A login lost after the model answered, or after other lines of the
		// run, reads like one refused at the start: only a run that cannot
		// have written the workspace may count it.
		{"agy answered, then lost its login", agy, answer, agyUnauthenticated, ErrorKindAuth, ErrorKindProvider},
		{"grok answered, then its session expired", grok, answer, grokExpired, ErrorKindAuth, ErrorKindProvider},
		{"muse partial answer, then logged out", muse, "partial answer\n", "not logged in: run /login to add an API key\n", ErrorKindAuth, ErrorKindProvider},
		{"agy answered, then rejected arguments", agy, answer, "Error: unknown flag: --effort\n", ErrorKindConfig, ErrorKindProvider},
		{"agy login lost after a tool line", agy, "", "Running go test ./router/...\n" + agyUnauthenticated, ErrorKindAuth, ErrorKindProvider},
		{"grok session expired after progress", grok, "", "Edited router/cli.go\n" + grokExpired, ErrorKindAuth, ErrorKindProvider},
		{"grok session prose after an answer line", grok, "", "I made the change.\nSession expired. Run `grok login` again is what the docs say.\nerror: turn failed\n", ErrorKindAuth, ErrorKindProvider},
		// The same words as task content, or a moot notice, are no refusal.
		{"tool output", agy, "", "Ran `tar -xzf a.tgz --strip`: tar: unknown option -- strip\nPartial changes written.\n", ErrorKindProvider, ErrorKindProvider},
		{"later line", agy, "", "I edited cli.py.\nerror: unknown flag: --json (from the gh call)\n", ErrorKindProvider, ErrorKindProvider},
		{"answer opening", agy, "", "Error: unknown flag: --json -- your gh is too old, so I rewrote scripts/ci.sh without it.\nPartial changes written.\n", ErrorKindProvider, ErrorKindProvider},
		{"moot start-up notice", agy, "", keyringOK + "You are not logged into Antigravity.\n", ErrorKindProvider, ErrorKindProvider},
		{"docs quote", agy, "", keyringOK + "Docs: users see 'You are not logged into Antigravity' until they sign in.\n", ErrorKindProvider, ErrorKindProvider},
		{"grok task failure", grok, "", "Error: internal failure in turn 3\n", ErrorKindProvider, ErrorKindProvider},
		// No login kind in the catalog: these CLIs' exits stay unrecognized.
		{"claude", NewClaudeCLI("unused"), "", "Not logged in · Please run /login\n", ErrorKindProvider, ErrorKindProvider},
		{"codex", NewCodexCLI("unused"), "", "Error: Not logged in\n", ErrorKindProvider, ErrorKindProvider},
		{"custom", NewCommandCLI("offline", "unused", nil, PromptModeArg), "", "Error: not logged in\n", ErrorKindProvider, ErrorKindProvider},
	} {
		for _, readOnly := range []bool{true, false} {
			want := tt.writes
			if readOnly {
				want = tt.readOnly
			}
			t.Run(fmt.Sprintf("%s/readOnly=%v", tt.name, readOnly), func(t *testing.T) {
				err := tt.cli.formatExecutionError(readOnly, []byte(tt.stdout), tt.stderr, "", exit, nil, time.Second)
				var perr *ProviderError
				if !errors.As(err, &perr) || perr.Kind != want || perr.Op != "CLI" || perr.Retryable {
					t.Fatalf("error = %#v, want a non-retryable CLI error of kind %s", err, want)
				}
				refused := want != ErrorKindProvider
				if outcome := providerOutcome(err, Usage{}); outcome.Known != refused {
					t.Fatalf("outcome %+v: known=%v, want %v", outcome, outcome.Known, refused)
				}
				// A refusal is a known FAILED, an argument rejection included,
				// as health.py reports it; an unrecognized exit of a read-only
				// run is unknown but does not stop fallback.
				if refused && execution.ErrorStatus(err) != execution.Failed {
					t.Fatalf("refusal status = %s, want FAILED", execution.ErrorStatus(err))
				}
				var readOnlyUnknown *readOnlyUnknownError
				if errors.As(err, &readOnlyUnknown) != (readOnly && !refused) || stopExecutionReplay(err) {
					t.Fatalf("read-only exit marking of %#v", err)
				}
				if want == ErrorKindConfig && strings.Contains(perr.Message, "I1012") {
					t.Fatalf("glog diagnostics leaked into the refusal: %q", perr.Message)
				}
			})
		}
	}
}

// A refusal never overrides a lost exit result or an account switch.
func TestCLIInvocationRefusalKeepsUnknownEvidence(t *testing.T) {
	grok := NewCommandCLI("grok", "unused", nil, PromptModeArg)
	login := "Error: Not logged in. Run `grok login`.\n"
	switched := errors.Join(errors.New("exit status 1"), &execution.UnknownOutcomeError{Err: errors.New("selected login changed during direct execution")})
	for _, readOnly := range []bool{true, false} {
		for _, runErr := range []error{exec.ErrWaitDelay, switched} {
			if err := grok.formatExecutionError(readOnly, nil, login, "", runErr, nil, time.Second); !errors.Is(err, execution.ErrUnknownOutcome) || !stopExecutionReplay(err) {
				t.Fatalf("readOnly=%v, run error %v: refusal hid an unknown outcome: %v", readOnly, runErr, err)
			}
		}
		if err := grok.formatExecutionError(readOnly, nil, login, "", errors.New("exit status 1"), context.DeadlineExceeded, time.Second); ErrorKindOf(err) != ErrorKindTimeout {
			t.Fatalf("readOnly=%v: deadline lost its priority: %v", readOnly, err)
		}
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

// The ensemble reports an unknown outcome in preference to a refusal, but a
// generator refused by accounting still ends ChatBest, whatever the other
// generators produced: no judge picks among the survivors.
func TestChatBestPowerAccountingRefusalBesideUnknownGeneratorEndsRequest(t *testing.T) {
	isolateCLITestHome(t)
	configDir := t.TempDir()
	overrides := `{"models":{"grok":{"cheap":"grok-x","mid":"grok-x","premium":"grok-x"},"muse":{"cheap":"muse-x","mid":"muse-x","premium":"muse-x"}},` +
		`"power_ensemble":{"code":{"generators":["claude","codex","grok","muse"],"judge":"gemini"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "routing.json"), []byte(overrides), 0o600); err != nil {
		t.Fatal(err)
	}
	failing := func(name string, err error) Provider {
		return &spendThenFailProvider{ensembleCaptureProvider: ensembleCaptureProvider{name: name, available: true}, err: err}
	}
	judge := &ensembleCaptureProvider{name: "gemini", available: true, content: "WINNER: 1"}
	providers := map[string]ProviderEntry{
		"claude": {Provider: &ensembleCaptureProvider{name: "claude", available: true, content: "claude answer"}, Access: AccessSubscription},
		"codex":  {Provider: &ensembleCaptureProvider{name: "codex", available: true, content: "codex answer"}, Access: AccessSubscription},
		"grok":   {Provider: failing("grok", fmt.Errorf("%w: synthetic ledger write failure", errExecutionAccounting)), Access: AccessSubscription},
		"muse":   {Provider: failing("muse", newProviderError("muse", "CLI", ErrorKindProvider, false, 0, "muse CLI exited 1", nil)), Access: AccessSubscription},
		"gemini": {Provider: judge, Access: AccessSubscription},
	}
	r, err := NewRouterFromConfig(RouterConfig{Providers: providers, UsageMode: "power", ConfigDir: configDir})
	if err != nil {
		t.Fatal(err)
	}
	out, _, _, err := r.ChatBest(context.Background(), PhaseCode, []Message{{Role: "user", Content: "implement it"}}, "")
	judge.mu.Lock()
	judged := judge.calls
	judge.mu.Unlock()
	if err == nil || out != "" || judged != 0 {
		t.Fatalf("ChatBest = %q err=%v judge calls=%d, want the request ended by the refusal", out, err, judged)
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
