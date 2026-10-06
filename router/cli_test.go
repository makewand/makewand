package router

import (
	"context"
	"errors"
	"fmt"
	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
	"github.com/makewand/makewand/internal/testfixture"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCLIProvider_ChatStream_ReturnsErrorOnExitFailure(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "output", Stdout: "stream-start\n", Stderr: "boom-on-stderr\n", ExitCode: 1})

	p := NewClaudeCLI(script)
	ch, err := p.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err != nil {
		t.Fatalf("ChatStream() start error = %v", err)
	}

	timeout := time.After(3 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatal("stream closed without emitting error")
			}
			if chunk.Error != nil {
				return
			}
			if chunk.Done {
				t.Fatal("stream reported Done without surfacing CLI exit error")
			}
		case <-timeout:
			t.Fatal("timed out waiting for stream error")
		}
	}
}

func TestCLIProvider_Chat_TimeoutErrorIsReadable(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Sleep: 2 * time.Second})

	p := NewClaudeCLI(script)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := p.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err == nil {
		t.Fatal("Chat() error = nil, want timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Chat() error = %q, want readable timeout message", err.Error())
	}
}

func TestShouldRejectCLIOutput_AuthPrompt(t *testing.T) {
	reject, reason := shouldRejectCLIOutput("return only final answer", "Opening authentication page in your browser. Do you want to continue? [Y/n]:")
	if !reject {
		t.Fatal("shouldRejectCLIOutput() = false, want true for auth prompt")
	}
	if !strings.Contains(strings.ToLower(reason), "authentication") {
		t.Fatalf("reason = %q, want auth-related message", reason)
	}
}

func TestShouldRejectCLIOutput_CodeOnlyRejectsMetaResponse(t *testing.T) {
	prompt := "Return only the complete content of solution.js. Do not output markdown. No explanations."
	content := "The file has been written to `solution.js`. Here's a summary of the implementation."

	reject, _ := shouldRejectCLIOutput(prompt, content)
	if !reject {
		t.Fatal("shouldRejectCLIOutput() = false, want true for code-only meta response")
	}
}

func TestShouldRejectCLIOutput_CodeOnlyAcceptsCode(t *testing.T) {
	prompt := "Return only the complete content of retry.go. Do not output markdown. No explanations."
	content := "package retrycase\n\nfunc RetryHTTP() {}\n"

	reject, _ := shouldRejectCLIOutput(prompt, content)
	if reject {
		t.Fatal("shouldRejectCLIOutput() = true, want false for valid code output")
	}
}

func TestShouldRejectCLIOutput_CodeOnlyRejectsWorkspaceDiscoveryResponse(t *testing.T) {
	prompt := "Rewrite pricing.py so python3 -m unittest -q passes. Output only Python source code for pricing.py. No markdown. No prose."
	content := "The listed files are not in `/workspace/project`; I'm locating the actual project directory and then I'll read the implementation and tests there."

	reject, _ := shouldRejectCLIOutput(prompt, content)
	if !reject {
		t.Fatal("shouldRejectCLIOutput() = false, want true for workspace-discovery meta response")
	}
}

func TestContainsLikelyCode_DoesNotTreatNaturalLanguageSemicolonAsCode(t *testing.T) {
	content := "The listed files are not in `/workspace/project`; I'm locating the actual project directory."
	if containsLikelyCode(content) {
		t.Fatal("containsLikelyCode() = true, want false for natural-language semicolon response")
	}
}

func TestCLIProvider_Chat_ValidationIgnoresSystemFileInstructions(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Stdout: "provider:reviewer\n"})

	p := NewCommandCLI("reviewer", script, nil, PromptModeStdin)
	system := "When creating or modifying files, use this format:\n--- FILE: path/to/file ---\n```\nfile content here\n```"

	content, _, err := p.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "please review."}},
		system,
		256,
	)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got := strings.TrimSpace(content); got != "provider:reviewer" {
		t.Fatalf("Chat() content = %q, want %q", got, "provider:reviewer")
	}
}

func TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline(t *testing.T) {
	t.Run("deadline_before_readiness", func(t *testing.T) {
		script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Sleep: 2 * time.Second})
		p := NewClaudeCLI(script)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		deadline, _ := ctx.Deadline()
		ch, startErr := p.ChatStream(ctx, []Message{{Role: "user", Content: "hi"}}, "", 256)
		checkTimeout := func(err error) {
			if ctx.Err() != context.DeadlineExceeded || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, execution.ErrUnknownOutcome) || ErrorKindOf(err) != ErrorKindTimeout {
				t.Fatalf("early stream deadline lost its typed timeout/unknown: err=%v ctx=%v", err, ctx.Err())
			}
			if time.Since(deadline) > 3*time.Second {
				t.Fatal("stream cleanup exceeded its existing bound after the real deadline")
			}
		}
		if startErr != nil {
			if ch != nil {
				t.Fatal("failed start returned a live stream")
			}
			// A genuine deadline may expire during native process initialization.
			// The synchronous path must preserve exactly the same typed outcome.
			checkTimeout(startErr)
			return
		}
		timer := time.NewTimer(max(0, time.Until(deadline.Add(3*time.Second))))
		defer timer.Stop()
		for {
			select {
			case chunk, ok := <-ch:
				if !ok {
					t.Fatal("stream closed without emitting timeout error")
				}
				if chunk.Error != nil {
					checkTimeout(chunk.Error)
					return
				}
			case <-timer.C:
				t.Fatal("timed out waiting for stream timeout error")
			}
		}
	})
	t.Run("cancel_after_real_readiness", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "active-descendant")
		p := cliProcessProvider(t, "quiet", marker)
		ctx, cancel := context.WithTimeout(context.Background(), cliProcessFixtureStartupTimeout)
		completed := make(chan struct{})
		var streamErr error
		go func() {
			defer close(completed)
			stream, err := p.ChatStream(ctx, []Message{{Role: "user", Content: "fixture"}}, "", 0)
			if err != nil {
				streamErr = err
				return
			}
			for chunk := range stream {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
		}()
		defer func() {
			cancel()
			select {
			case <-completed:
			case <-time.After(2 * time.Second):
				t.Error("active stream did not finish after fixture cancellation")
			}
		}()
		if err := waitCLIProcessFixtureReady(ctx, marker, completed); err != nil {
			t.Fatalf("actual child did not reach readiness: %v", err)
		}
		readyAt := time.Now()
		// Initialization has completed: exercise a real active stream for the
		// original short interval, then cancel its genuine parent context.
		time.Sleep(100 * time.Millisecond)
		cancelledAt := time.Now()
		cancel()
		select {
		case <-completed:
		case <-time.After(1800 * time.Millisecond):
			t.Fatal("ready stream exceeded its cancellation/pipe cleanup bound")
		}
		if time.Since(cancelledAt) > 1800*time.Millisecond || ctx.Err() != context.Canceled || (streamErr != nil && !errors.Is(streamErr, context.Canceled)) {
			t.Fatalf("ready stream lost cancellation: error=%v ctx=%v cleanup=%v", streamErr, ctx.Err(), time.Since(cancelledAt))
		}
		if remaining := time.Until(readyAt.Add(2300 * time.Millisecond)); remaining > 0 {
			time.Sleep(remaining)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("actual descendant survived stream cancellation: %v", err)
		}
	})
}

func TestGeminiCLI_ChatStream_ParsesStreamJSON(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Stdout: "{\"type\":\"init\",\"session_id\":\"s\",\"model\":\"gemini\"}\n{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"Hello\"}\n{\"type\":\"message\",\"role\":\"assistant\",\"content\":\" world\"}\n{\"type\":\"result\",\"status\":\"success\"}\n"})

	p := NewGeminiCLI(script)
	ch, err := p.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err != nil {
		t.Fatalf("ChatStream() start error = %v", err)
	}

	var (
		parts []string
		done  bool
	)
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error = %v", chunk.Error)
		}
		if chunk.Content != "" {
			parts = append(parts, chunk.Content)
		}
		if chunk.Done {
			done = true
		}
	}

	if got := strings.Join(parts, ""); got != "Hello world" {
		t.Fatalf("stream content = %q, want %q", got, "Hello world")
	}
	if !done {
		t.Fatal("stream did not emit Done")
	}
}

func TestGeminiCLI_ChatStream_IgnoresPlaintextPrelude(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Stdout: "Loaded cached credentials.\n{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"OK\"}\n{\"type\":\"result\",\"status\":\"success\"}\n"})

	p := NewGeminiCLI(script)
	ch, err := p.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err != nil {
		t.Fatalf("ChatStream() start error = %v", err)
	}

	var (
		content string
		done    bool
	)
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error = %v", chunk.Error)
		}
		content += chunk.Content
		if chunk.Done {
			done = true
		}
	}

	if content != "OK" {
		t.Fatalf("stream content = %q, want %q", content, "OK")
	}
	if !done {
		t.Fatal("stream did not emit Done")
	}
}

func TestNewCommandCLI_PromptPlaceholderReplacement(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "echo-args"})

	p := NewCommandCLI("private", script, []string{"--prompt", "{{prompt}}"}, "legacy")
	content, usage, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hello custom provider"}}, "", 256)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !strings.Contains(content, "--prompt") {
		t.Fatalf("Chat() content = %q, want args echoed", content)
	}
	if !strings.Contains(content, "hello custom provider") {
		t.Fatalf("Chat() content = %q, want replaced prompt", content)
	}
	if strings.Contains(content, "{{prompt}}") {
		t.Fatalf("Chat() content = %q, should not contain placeholder token", content)
	}
	if usage.Provider != "private" {
		t.Fatalf("usage.Provider = %q, want %q", usage.Provider, "private")
	}
}

func TestNewCommandCLI_AppendsPromptWhenNoPlaceholder(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "echo-args"})

	p := NewCommandCLI("private", script, []string{"--flag"}, "legacy")
	content, _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hello appended prompt"}}, "", 256)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !strings.Contains(content, "--flag") {
		t.Fatalf("Chat() content = %q, want fixed arg", content)
	}
	if !strings.Contains(content, "hello appended prompt") {
		t.Fatalf("Chat() content = %q, want prompt appended as arg", content)
	}
}

func TestNewCommandCLI_WritesPromptToStdinWhenConfigured(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "echo-stdin"})

	p := NewCommandCLI("private", script, []string{"stdin"}, "stdin")
	content, _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hello via stdin"}}, "", 256)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !strings.Contains(content, "mode:stdin") {
		t.Fatalf("Chat() content = %q, want fixed arg echoed", content)
	}
	if !strings.Contains(content, "hello via stdin") {
		t.Fatalf("Chat() content = %q, want prompt from stdin", content)
	}
}

func TestCLIProvider_Chat_RetriesExplicitQuotaRejection(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "attempts.txt")
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "counter", StateFile: stateFile, FirstError: "quota exceeded", Stdout: "ok after retry\n", Stderr: "", ExitCode: 0})

	p := NewCommandCLI("private", script, []string{stateFile}, "legacy")
	content, _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err != nil {
		t.Fatalf("Chat() error = %v, want retry success", err)
	}
	if !strings.Contains(content, "ok after retry") {
		t.Fatalf("Chat() content = %q, want retry success output", content)
	}

	attemptsData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(stateFile): %v", err)
	}
	attempts, convErr := strconv.Atoi(strings.TrimSpace(string(attemptsData)))
	if convErr != nil {
		t.Fatalf("Atoi(attempts): %v", convErr)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry)", attempts)
	}
}

func TestCLIProvider_Chat_DoesNotRetryNonTransientError(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "attempts.txt")
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "counter", StateFile: stateFile, FirstError: "", Stdout: "", Stderr: "invalid_api_key\n", ExitCode: 1})

	p := NewCommandCLI("private", script, []string{stateFile}, "legacy")
	_, _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err == nil {
		t.Fatal("Chat() error = nil, want permanent failure")
	}
	if !strings.Contains(err.Error(), "invalid_api_key") {
		t.Fatalf("Chat() error = %q, want surfaced stderr", err.Error())
	}

	attemptsData, readErr := os.ReadFile(stateFile)
	if readErr != nil {
		t.Fatalf("ReadFile(stateFile): %v", readErr)
	}
	attempts, convErr := strconv.Atoi(strings.TrimSpace(string(attemptsData)))
	if convErr != nil {
		t.Fatalf("Atoi(attempts): %v", convErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry for permanent error)", attempts)
	}
}

func TestCLIProvider_IsAvailable_UsesHealthProbe(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "probe", Stdout: "ok\n"})

	p := NewClaudeCLI(script)
	if !p.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true for healthy probe")
	}
}

func TestCLIProvider_IsAvailable_RejectsNonzeroProbe(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "probe", Stderr: "probe failed\n", ExitCode: 1})
	if p := NewCodexCLI(script); p.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false for a completed nonzero probe")
	}
}

func TestCLIProvider_IsAvailable_FailsWhenProbeHangs(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Sleep: 10 * time.Second})

	p := &CLIProvider{
		name:     "hang-cli",
		binPath:  script,
		provider: "hang",
		checkCmd: func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, script)
		},
	}

	start := time.Now()
	if p.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false when probe hangs")
	}
	elapsed := time.Since(start)
	if elapsed > cliAvailProbeTimeout+2*time.Second {
		t.Fatalf("IsAvailable() took too long: %s", elapsed)
	}
}

func TestCLIProvider_IsAvailable_SoftPassesTimeoutForGemini(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Sleep: 10 * time.Second})

	p := &CLIProvider{
		name:     "gemini-cli",
		binPath:  script,
		provider: "gemini",
		checkCmd: func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, script)
		},
	}

	start := time.Now()
	if !p.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true for gemini probe timeout soft-pass")
	}
	elapsed := time.Since(start)
	if elapsed > cliAvailProbeTimeout+2*time.Second {
		t.Fatalf("IsAvailable() took too long: %s", elapsed)
	}
}

func TestCLIProvider_HealthProbeDeadlineAndCompletionReady(t *testing.T) {
	// A real expired context and a buffered Wait result are both ready before
	// the production wait starts. The result must hold regardless of select's
	// scheduling, and every iteration must consume the completion result.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, provider := range []string{"claude", "codex", "gemini", "muse", "grok", "custom"} {
		t.Run(provider, func(t *testing.T) {
			p := &CLIProvider{provider: provider}
			want := provider != "custom"
			for attempt := 0; attempt < 128; attempt++ {
				done := make(chan error, 1)
				done <- errors.New("probe killed at deadline")
				capture := processjob.NewCapture(64, nil)
				stops := 0
				got := p.waitHealthProbe(ctx, done, capture, func() { stops++ })
				if got != want {
					t.Fatalf("both-ready attempt %d availability = %t, want %t", attempt, got, want)
				}
				if len(done) != 0 || stops > 1 {
					t.Fatalf("probe completion was not consumed once: pending=%d stops=%d", len(done), stops)
				}
			}
		})
	}
}

func TestCLIProvider_HealthProbeWaitPreservesFailureCases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		end      string
		err      error
		overflow bool
		want     bool
	}{
		{name: "healthy", provider: "codex", want: true},
		{name: "real nonzero exit", provider: "codex", err: errors.New("exit status 1")},
		{name: "canceled subscription", provider: "codex", end: "cancel", err: context.Canceled},
		{name: "custom deadline", provider: "custom", end: "deadline", err: context.DeadlineExceeded},
		{name: "deadline output overflow", provider: "codex", end: "deadline", err: context.DeadlineExceeded, overflow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			switch tc.end {
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			capture := processjob.NewCapture(1, nil)
			if tc.overflow {
				if _, err := capture.Stdout().Write([]byte("xx")); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			if ctx.Err() == nil {
				done <- tc.err
			}
			stopped := false
			p := &CLIProvider{provider: tc.provider}
			got := p.waitHealthProbe(ctx, done, capture, func() {
				stopped = true
				done <- tc.err
			})
			if got != tc.want || len(done) != 0 || stopped != (ctx.Err() != nil) {
				t.Fatalf("availability=%t want=%t pending=%d stopped=%t ctx=%v", got, tc.want, len(done), stopped, ctx.Err())
			}
		})
	}
}

func TestTransientCLIError_IsDetectedByIsTransient(t *testing.T) {
	base := newProviderError("test", "CLI", ErrorKindNetwork, true, 0, "some error", fmt.Errorf("some error"))
	transient := &TransientCLIError{Err: base}
	wrapped := fmt.Errorf("wrapped: %w", transient)

	if !isTransientCLIError(transient) {
		t.Fatal("isTransientCLIError(TransientCLIError) = false, want true")
	}
	if !isTransientCLIError(wrapped) {
		t.Fatal("isTransientCLIError(wrapped TransientCLIError) = false, want true")
	}
}

func TestTransientCLIError_ContextErrorsAreNotTransient(t *testing.T) {
	if isTransientCLIError(context.Canceled) {
		t.Fatal("context.Canceled should not be transient")
	}
	if isTransientCLIError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded should not be transient")
	}
}

func TestEstimateTokens_HandlesMultipleScripts(t *testing.T) {
	// English text: ~1.3 tokens/word
	english := "The quick brown fox jumps over the lazy dog"
	got := estimateTokens(english)
	if got < 9 || got > 20 {
		t.Fatalf("estimateTokens(english) = %d, want 9-20", got)
	}

	// CJK text should use rune count rather than UTF-8 byte length.
	cjk := "这是一个简单的测试文本用来验证中文分词估算"
	got = estimateTokens(cjk)
	oldByteBased := int(float64(len(cjk)) / 3.5)
	if got >= oldByteBased {
		t.Fatalf("estimateTokens(cjk) = %d, want less than old byte-based estimate %d", got, oldByteBased)
	}
	if got < 5 || got > 10 {
		t.Fatalf("estimateTokens(cjk) = %d, want 5-10 with rune-based estimate", got)
	}

	// Empty
	if estimateTokens("") != 0 {
		t.Fatal("estimateTokens('') should be 0")
	}
}

func TestFormatCLIExecutionError_MarksTransientErrors(t *testing.T) {
	err := formatCLIExecutionError("test", "stream closed unexpectedly", fmt.Errorf("exit 1"), nil, time.Second)
	var transient *TransientCLIError
	if !errors.As(err, &transient) {
		t.Fatalf("expected TransientCLIError for transient stderr, got %T: %v", err, err)
	}
	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProviderError for transient stderr, got %T: %v", err, err)
	}
	if perr.Kind != ErrorKindNetwork || !perr.Retryable {
		t.Fatalf("ProviderError = %+v, want Kind=network Retryable=true", perr)
	}
}

func TestFormatCLIExecutionError_NonTransientStaysPlain(t *testing.T) {
	err := formatCLIExecutionError("test", "invalid_api_key", fmt.Errorf("exit 1"), nil, time.Second)
	var transient *TransientCLIError
	if errors.As(err, &transient) {
		t.Fatalf("expected plain error for non-transient stderr, got TransientCLIError: %v", err)
	}
	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProviderError for non-transient stderr, got %T: %v", err, err)
	}
	if perr.Kind != ErrorKindProvider || perr.Retryable {
		t.Fatalf("ProviderError = %+v, want Kind=provider Retryable=false", perr)
	}
}

func TestApplyGeminiProxyPolicy_DefaultRespectsConfiguredProxy(t *testing.T) {
	t.Setenv("MAKEWAND_GEMINI_USE_PROXY", "")
	t.Setenv("MAKEWAND_GEMINI_BYPASS_PROXY", "")

	env := []string{"HTTP_PROXY=http://127.0.0.1:7890"}
	got := applyGeminiProxyPolicy(env)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "NO_PROXY=googleapis.com") || strings.Contains(joined, "no_proxy=googleapis.com") {
		t.Fatalf("applyGeminiProxyPolicy() should not force NO_PROXY when proxy is configured; got %q", joined)
	}
}

func TestApplyGeminiProxyPolicy_DefaultBypassesWhenNoProxyConfigured(t *testing.T) {
	t.Setenv("MAKEWAND_GEMINI_USE_PROXY", "")
	t.Setenv("MAKEWAND_GEMINI_BYPASS_PROXY", "")

	got := applyGeminiProxyPolicy([]string{"PATH=/usr/bin"})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "NO_PROXY=googleapis.com") && !strings.Contains(joined, "no_proxy=googleapis.com") {
		t.Fatalf("applyGeminiProxyPolicy() should append NO_PROXY when no proxy is configured; got %q", joined)
	}
}

func TestApplyGeminiProxyPolicy_ExplicitBypassOverridesProxy(t *testing.T) {
	t.Setenv("MAKEWAND_GEMINI_USE_PROXY", "")
	t.Setenv("MAKEWAND_GEMINI_BYPASS_PROXY", "1")

	got := applyGeminiProxyPolicy([]string{"HTTP_PROXY=http://127.0.0.1:7890"})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "NO_PROXY=googleapis.com") && !strings.Contains(joined, "no_proxy=googleapis.com") {
		t.Fatalf("applyGeminiProxyPolicy() should force NO_PROXY when bypass is set; got %q", joined)
	}
}

// --- JSON Output Parsing Tests ---

func TestParseClaudeCLIJSON_ValidResponse(t *testing.T) {
	raw := []byte(`{
		"type": "result",
		"subtype": "success",
		"result": "Hello! How can I help you today?",
		"total_cost_usd": 0.10739,
		"usage": {
			"input_tokens": 3,
			"output_tokens": 12
		},
		"modelUsage": {
			"claude-opus-4-6": {
				"inputTokens": 3,
				"outputTokens": 12,
				"costUSD": 0.10739
			}
		}
	}`)

	content, usage, err := parseClaudeCLIJSON(raw)
	if err != nil {
		t.Fatalf("parseClaudeCLIJSON() error = %v", err)
	}
	if content != "Hello! How can I help you today?" {
		t.Fatalf("content = %q, want greeting", content)
	}
	if usage == nil {
		t.Fatal("usage = nil, want non-nil")
	}
	if usage.InputTokens != 3 {
		t.Fatalf("InputTokens = %d, want 3", usage.InputTokens)
	}
	if usage.OutputTokens != 12 {
		t.Fatalf("OutputTokens = %d, want 12", usage.OutputTokens)
	}
	if usage.Cost < 0.10 {
		t.Fatalf("Cost = %f, want >= 0.10", usage.Cost)
	}
	if usage.Model != "claude-opus-4-6" {
		t.Fatalf("Model = %q, want claude-opus-4-6", usage.Model)
	}
	if usage.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", usage.Provider)
	}
}

func TestParseClaudeCLIJSON_MinimalResponse(t *testing.T) {
	raw := []byte(`{"result": "Hi", "total_cost_usd": 0.001, "usage": {"input_tokens": 1, "output_tokens": 2}}`)
	content, usage, err := parseClaudeCLIJSON(raw)
	if err != nil {
		t.Fatalf("parseClaudeCLIJSON() error = %v", err)
	}
	if content != "Hi" {
		t.Fatalf("content = %q, want Hi", content)
	}
	if usage.InputTokens != 1 || usage.OutputTokens != 2 {
		t.Fatalf("usage tokens = %d/%d, want 1/2", usage.InputTokens, usage.OutputTokens)
	}
	if usage.Cost != 0.001 {
		t.Fatalf("Cost = %f, want 0.001", usage.Cost)
	}
}

func TestParseClaudeCLIJSON_EmptyResult(t *testing.T) {
	raw := []byte(`{"result": "", "usage": {"input_tokens": 1, "output_tokens": 0}}`)
	_, _, err := parseClaudeCLIJSON(raw)
	if err == nil {
		t.Fatal("parseClaudeCLIJSON() error = nil, want error for empty result")
	}
}

func TestParseClaudeCLIJSON_InvalidJSON(t *testing.T) {
	_, _, err := parseClaudeCLIJSON([]byte(`not json at all`))
	if err == nil {
		t.Fatal("parseClaudeCLIJSON() error = nil, want error for invalid JSON")
	}
}

func TestParseGeminiCLIJSON_ValidResponse(t *testing.T) {
	raw := []byte(`{
		"session_id": "abc123",
		"response": "Hello. I am ready to assist.",
		"stats": {
			"models": {
				"gemini-2.5-flash-lite": {
					"tokens": {
						"input": 3280,
						"candidates": 27,
						"total": 3442
					}
				},
				"gemini-3-flash-preview": {
					"tokens": {
						"input": 7346,
						"candidates": 13,
						"total": 7853
					}
				}
			}
		}
	}`)

	content, usage, err := parseGeminiCLIJSON(raw)
	if err != nil {
		t.Fatalf("parseGeminiCLIJSON() error = %v", err)
	}
	if content != "Hello. I am ready to assist." {
		t.Fatalf("content = %q, want greeting", content)
	}
	if usage == nil {
		t.Fatal("usage = nil, want non-nil")
	}
	// Aggregated across both models: 3280 + 7346 = 10626
	if usage.InputTokens != 10626 {
		t.Fatalf("InputTokens = %d, want 10626", usage.InputTokens)
	}
	// Aggregated output: 27 + 13 = 40
	if usage.OutputTokens != 40 {
		t.Fatalf("OutputTokens = %d, want 40", usage.OutputTokens)
	}
	if usage.Cost != 0 {
		t.Fatalf("Cost = %f, want 0 (free tier)", usage.Cost)
	}
	if usage.Provider != "gemini" {
		t.Fatalf("Provider = %q, want gemini", usage.Provider)
	}
}

func TestParseGeminiCLIJSON_SingleModel(t *testing.T) {
	raw := []byte(`{
		"response": "Done.",
		"stats": {
			"models": {
				"gemini-2.5-pro": {
					"tokens": {"input": 500, "candidates": 100, "total": 600}
				}
			}
		}
	}`)

	content, usage, err := parseGeminiCLIJSON(raw)
	if err != nil {
		t.Fatalf("parseGeminiCLIJSON() error = %v", err)
	}
	if content != "Done." {
		t.Fatalf("content = %q, want Done.", content)
	}
	if usage.InputTokens != 500 || usage.OutputTokens != 100 {
		t.Fatalf("usage tokens = %d/%d, want 500/100", usage.InputTokens, usage.OutputTokens)
	}
	if usage.Model != "gemini-2.5-pro" {
		t.Fatalf("Model = %q, want gemini-2.5-pro", usage.Model)
	}
}

func TestParseGeminiCLIJSON_EmptyResponse(t *testing.T) {
	raw := []byte(`{"response": "", "stats": {"models": {}}}`)
	_, _, err := parseGeminiCLIJSON(raw)
	if err == nil {
		t.Fatal("parseGeminiCLIJSON() error = nil, want error for empty response")
	}
}

func TestParseGeminiCLIJSON_InvalidJSON(t *testing.T) {
	_, _, err := parseGeminiCLIJSON([]byte(`{broken`))
	if err == nil {
		t.Fatal("parseGeminiCLIJSON() error = nil, want error for invalid JSON")
	}
}

func TestCLIProvider_Chat_JSONFallbackToText(t *testing.T) {
	// Simulate a CLI that returns plain text even though jsonOutput is enabled.
	// The provider should fall back to text parsing gracefully.
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Stdout: "Hello from plain text\n"})

	p := &CLIProvider{
		name:              "test-json-fallback",
		binPath:           script,
		provider:          "testprov",
		jsonOutput:        true,
		parseJSONResponse: parseClaudeCLIJSON, // Will fail on plain text
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		return exec.CommandContext(ctx, script)
	}

	content, usage, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err != nil {
		t.Fatalf("Chat() error = %v, want fallback success", err)
	}
	if !strings.Contains(content, "Hello from plain text") {
		t.Fatalf("content = %q, want plain text output", content)
	}
	// Fallback should use estimated tokens, not JSON usage.
	if usage.InputTokens == 0 {
		t.Fatal("usage.InputTokens = 0, want estimated tokens from fallback")
	}
	if usage.Provider != "testprov" {
		t.Fatalf("usage.Provider = %q, want testprov", usage.Provider)
	}
}

func TestCLIProvider_Chat_JSONUsageUsedWhenAvailable(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Stdout: "{\"result\":\"JSON hello\",\"total_cost_usd\":0.05,\"usage\":{\"input_tokens\":10,\"output_tokens\":5},\"modelUsage\":{\"test-model\":{\"inputTokens\":10,\"outputTokens\":5,\"costUSD\":0.05}}}\n"})

	p := &CLIProvider{
		name:              "test-json-cli",
		binPath:           script,
		provider:          "claude",
		jsonOutput:        true,
		parseJSONResponse: parseClaudeCLIJSON,
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		return exec.CommandContext(ctx, script)
	}

	content, usage, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if content != "JSON hello" {
		t.Fatalf("content = %q, want JSON hello", content)
	}
	if usage.InputTokens != 10 {
		t.Fatalf("InputTokens = %d, want 10 (from JSON)", usage.InputTokens)
	}
	if usage.OutputTokens != 5 {
		t.Fatalf("OutputTokens = %d, want 5 (from JSON)", usage.OutputTokens)
	}
	if usage.Cost != 0.05 {
		t.Fatalf("Cost = %f, want 0.05 (from JSON)", usage.Cost)
	}
	if usage.Model != "test-model" {
		t.Fatalf("Model = %q, want test-model", usage.Model)
	}
}

func TestParseCodexCLIJSONL_ValidResponse(t *testing.T) {
	// Real JSONL output format from `codex exec --json --skip-git-repo-check`
	jsonl := `{"type":"thread.started","thread_id":"019cd704-eb4e-7420-b45e-3f546e2456e0"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Hello from Codex!"}}
{"type":"turn.completed","usage":{"input_tokens":42,"cached_input_tokens":10,"output_tokens":15}}
`
	content, usage, err := parseCodexCLIJSONL([]byte(jsonl))
	if err != nil {
		t.Fatalf("parseCodexCLIJSONL() error = %v", err)
	}
	if content != "Hello from Codex!" {
		t.Fatalf("content = %q, want %q", content, "Hello from Codex!")
	}
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 42 {
		t.Fatalf("InputTokens = %d, want 42", usage.InputTokens)
	}
	if usage.OutputTokens != 15 {
		t.Fatalf("OutputTokens = %d, want 15", usage.OutputTokens)
	}
	if usage.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", usage.Provider)
	}
}

func TestParseCodexCLIJSONL_NoAgentMessages(t *testing.T) {
	jsonl := `{"type":"thread.started","thread_id":"abc"}
{"type":"turn.started"}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}
`
	_, _, err := parseCodexCLIJSONL([]byte(jsonl))
	if err == nil {
		t.Fatal("expected error for missing agent_message items")
	}
}

func TestParseCodexCLIJSONL_PlainTextFallback(t *testing.T) {
	// Plain text (e.g. from codex review) should fail JSONL parsing,
	// triggering the text fallback in chatAttempt.
	plain := "Code review: looks good, no issues found."
	_, _, err := parseCodexCLIJSONL([]byte(plain))
	if err == nil {
		t.Fatal("expected error for plain text input")
	}
}

func TestParseCodexCLIJSONL_MultipleAgentMessages(t *testing.T) {
	jsonl := `{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Part 1"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"echo hi","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"Part 2"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":20}}
`
	content, usage, err := parseCodexCLIJSONL([]byte(jsonl))
	if err != nil {
		t.Fatalf("parseCodexCLIJSONL() error = %v", err)
	}
	if content != "Part 1\nPart 2" {
		t.Fatalf("content = %q, want %q", content, "Part 1\nPart 2")
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 20 {
		t.Fatalf("usage = %+v, want input=10 output=20", usage)
	}
}

func TestContextWithTask_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := TaskFromContext(ctx); ok {
		t.Fatal("expected no task in bare context")
	}

	ctx = ContextWithTask(ctx, TaskReview)
	task, ok := TaskFromContext(ctx)
	if !ok {
		t.Fatal("expected task in context")
	}
	if task != TaskReview {
		t.Fatalf("task = %d, want TaskReview (%d)", task, TaskReview)
	}
}

func TestNewCodexCLI_ReviewTaskUsesReviewCmd(t *testing.T) {
	p := NewCodexCLI("/usr/bin/codex")
	// Review task → should build "codex review --uncommitted"
	ctx := ContextWithTask(context.Background(), TaskReview)
	cmd := p.buildCmd(ctx, "review this code")
	args := cmd.Args
	if len(args) < 3 || args[1] != "review" || args[2] != "--uncommitted" {
		t.Fatalf("review task: args = %v, want codex review invocation", args)
	}

	// Code task → should build "codex exec --json ..."
	ctx = ContextWithTask(context.Background(), TaskCode)
	cmd = p.buildCmd(ctx, "write hello world")
	args = cmd.Args
	if len(args) < 8 || args[1] != "exec" || args[2] != "--sandbox" || args[3] != "read-only" || args[4] != "--ephemeral" || args[5] != "--json" || args[6] != "--skip-git-repo-check" {
		t.Fatalf("code task: args = %v, want read-only codex exec invocation", args)
	}

	ctx = ContextWithWorkDir(ctx, t.TempDir())
	cmd = p.buildCmd(ctx, "write hello world")
	args = cmd.Args
	if len(args) < 8 || args[1] != "exec" || args[2] != "--sandbox" || args[3] != "workspace-write" || args[4] != "--ephemeral" || args[5] != "--json" || args[6] != "--skip-git-repo-check" {
		t.Fatalf("code task with workdir: args = %v, want workspace-write codex exec invocation", args)
	}
}

func TestNewClaudeCLI_ModelSelection(t *testing.T) {
	p := NewClaudeCLI("/usr/bin/claude")
	// With model in context → should add --model
	ctx := ContextWithModel(context.Background(), "claude-haiku-4-5-20251001")
	cmd := p.buildCmd(ctx, "hello")
	found := false
	for i, arg := range cmd.Args {
		if arg == "--model" && i+1 < len(cmd.Args) && cmd.Args[i+1] == "claude-haiku-4-5-20251001" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected --model claude-haiku-4-5-20251001 in args: %v", cmd.Args)
	}

	// Without model in context → no --model
	ctx = context.Background()
	cmd = p.buildCmd(ctx, "hello")
	for _, arg := range cmd.Args {
		if arg == "--model" {
			t.Fatalf("unexpected --model in args without context: %v", cmd.Args)
		}
	}

	// With -cli suffix model → should NOT add --model (placeholder)
	ctx = ContextWithModel(context.Background(), "codex-cli")
	cmd = p.buildCmd(ctx, "hello")
	for _, arg := range cmd.Args {
		if arg == "--model" {
			t.Fatalf("unexpected --model for -cli suffix: %v", cmd.Args)
		}
	}
}

func TestNewGeminiCLI_ModelSelection(t *testing.T) {
	p := NewGeminiCLI("/usr/bin/gemini")
	ctx := ContextWithModel(context.Background(), "gemini-2.5-pro")
	cmd := p.buildCmd(ctx, "hello")
	found := false
	for i, arg := range cmd.Args {
		if arg == "--model" && i+1 < len(cmd.Args) && cmd.Args[i+1] == "gemini-2.5-pro" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected --model gemini-2.5-pro in args: %v", cmd.Args)
	}
}

func TestNewAgyCLI_ArgumentOrder(t *testing.T) {
	t.Run("uses configured default and keeps print paired with prompt", func(t *testing.T) {
		t.Setenv("MAKEWAND_AGY_MODEL", "")

		p := NewAgyCLI("/usr/bin/agy")
		// Agy's model namespace can span several quota pools and is not compatible
		// with makewand's Gemini tier IDs, so a routing-context model is deliberately
		// ignored unless the user opts in through MAKEWAND_AGY_MODEL.
		ctx := ContextWithModel(context.Background(), "gemini-2.5-pro")
		prompt := "reply with MW_AGY_PROMPT"
		want := []string{"/usr/bin/agy", "--sandbox", "--print", prompt}

		for name, build := range map[string]func(context.Context, string) *exec.Cmd{
			"chat":   p.buildCmd,
			"stream": p.buildStreamCmd,
		} {
			t.Run(name, func(t *testing.T) {
				got := build(ctx, prompt).Args
				if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
					t.Fatalf("args = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("environment override is a global option before print", func(t *testing.T) {
		t.Setenv("MAKEWAND_AGY_MODEL", "  agy-model-id  ")

		p := NewAgyCLI("/usr/bin/agy")
		prompt := "reply with MW_AGY_PROMPT"
		got := p.buildCmd(context.Background(), prompt).Args
		want := []string{
			"/usr/bin/agy",
			"--sandbox",
			"--model", "agy-model-id",
			"--print", prompt,
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args = %q, want %q", got, want)
		}
	})
}

func TestAgyCLI_ChatPassesActualUserPrompt(t *testing.T) {
	t.Setenv("MAKEWAND_AGY_MODEL", "")

	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "agy-prompt"})

	p := NewAgyCLI(script)
	want := "MW_AGY_PROMPT_SENTINEL"
	content, _, err := p.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: want}},
		"",
		256,
	)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if content != want {
		t.Fatalf("Chat() content = %q, want actual user prompt %q", content, want)
	}
}

func TestCLIProvider_ReportedModelID(t *testing.T) {
	t.Setenv("MAKEWAND_AGY_MODEL", "")
	if got := NewAgyCLI("agy").ReportedModelID("gemini-2.5-pro"); got != "agy-provider-managed" {
		t.Fatalf("Agy ReportedModelID = %q, want provider-managed identity", got)
	}
	t.Setenv("MAKEWAND_AGY_MODEL", "gemini-override")
	if got := NewAgyCLI("agy").ReportedModelID("gemini-2.5-pro"); got != "gemini-override" {
		t.Fatalf("Agy ReportedModelID with override = %q, want gemini-override", got)
	}
	if got := NewCodexCLI("codex").ReportedModelID("codex-cli"); got != "codex-provider-managed" {
		t.Fatalf("Codex ReportedModelID = %q, want provider-managed identity", got)
	}
	if got := NewClaudeCLI("claude").ReportedModelID("claude-opus"); got != "claude-opus" {
		t.Fatalf("Claude ReportedModelID = %q, want requested model", got)
	}
}

func TestNewClaudeCLI_SystemPromptFlag(t *testing.T) {
	p := NewClaudeCLI("/usr/bin/claude")
	ctx := ContextWithSystem(context.Background(), "You are a helpful assistant")
	cmd := p.buildCmd(ctx, "hello")
	found := false
	for i, arg := range cmd.Args {
		if arg == "--append-system-prompt" && i+1 < len(cmd.Args) && cmd.Args[i+1] == "You are a helpful assistant" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected --append-system-prompt in args: %v", cmd.Args)
	}
}

func TestClaudeCLI_Chat_SystemPromptSeparation(t *testing.T) {
	script := testfixture.WriteCLI(t, "controlled-cli", testfixture.Spec{Mode: "echo-args"})

	p := NewClaudeCLI(script)
	content, _, err := p.Chat(context.Background(),
		[]Message{{Role: "user", Content: "test prompt"}},
		"system instructions here",
		256,
	)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	// The output should contain --append-system-prompt and the system text
	if !strings.Contains(content, "--append-system-prompt") {
		t.Fatalf("expected --append-system-prompt in CLI args output: %q", content)
	}
	if !strings.Contains(content, "system instructions here") {
		t.Fatalf("expected system prompt in CLI args output: %q", content)
	}
	// The prompt itself should NOT contain the system instructions
	// (they're passed via the flag, not concatenated)
	if strings.Contains(content, "system instructions here\n\ntest prompt") {
		t.Fatal("system should not be concatenated into prompt when systemFlag is set")
	}
}
