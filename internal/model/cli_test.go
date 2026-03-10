package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/config"
)

func TestCLIProvider_ChatStream_ReturnsErrorOnExitFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail-cli.sh")
	body := "#!/bin/sh\n" +
		"echo stream-start\n" +
		"echo boom-on-stderr 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

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
	dir := t.TempDir()
	script := filepath.Join(dir, "sleep-cli.sh")
	body := "#!/bin/sh\n" +
		"sleep 2\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

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

func TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sleep-stream-cli.sh")
	body := "#!/bin/sh\n" +
		"sleep 2\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	p := NewClaudeCLI(script)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	ch, err := p.ChatStream(ctx, []Message{{Role: "user", Content: "hi"}}, "", 256)
	if err != nil {
		t.Fatalf("ChatStream() start error = %v", err)
	}

	timeout := time.After(3 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatal("stream closed without emitting timeout error")
			}
			if chunk.Error != nil {
				if !strings.Contains(chunk.Error.Error(), "timeout") {
					t.Fatalf("stream error = %q, want timeout message", chunk.Error.Error())
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for stream timeout error")
		}
	}
}

func TestNewCommandCLI_PromptPlaceholderReplacement(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-args.sh")
	body := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  printf '%s\\n' \"$arg\"\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	p := NewCommandCLI("private", script, []string{"--prompt", "{{prompt}}"}, config.CustomPromptModeLegacy)
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
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-last.sh")
	body := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  printf '%s\\n' \"$arg\"\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	p := NewCommandCLI("private", script, []string{"--flag"}, config.CustomPromptModeLegacy)
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
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-stdin.sh")
	body := "#!/bin/sh\n" +
		"printf 'mode:%s\\n' \"$1\"\n" +
		"cat\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	p := NewCommandCLI("private", script, []string{"stdin"}, config.CustomPromptModeStdin)
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

func TestCLIProvider_Chat_RetriesTransientExecutionError(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "attempts.txt")
	script := filepath.Join(dir, "flaky-cli.sh")
	body := "#!/bin/sh\n" +
		"state_file=\"$1\"\n" +
		"if [ -f \"$state_file\" ]; then\n" +
		"  n=$(cat \"$state_file\")\n" +
		"else\n" +
		"  n=0\n" +
		"fi\n" +
		"n=$((n+1))\n" +
		"echo \"$n\" > \"$state_file\"\n" +
		"if [ \"$n\" -eq 1 ]; then\n" +
		"  echo \"stream closed unexpectedly: Transport error (1007)\" 1>&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo \"ok after retry\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	p := NewCommandCLI("private", script, []string{stateFile}, config.CustomPromptModeLegacy)
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
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "attempts.txt")
	script := filepath.Join(dir, "fail-permanent-cli.sh")
	body := "#!/bin/sh\n" +
		"state_file=\"$1\"\n" +
		"if [ -f \"$state_file\" ]; then\n" +
		"  n=$(cat \"$state_file\")\n" +
		"else\n" +
		"  n=0\n" +
		"fi\n" +
		"n=$((n+1))\n" +
		"echo \"$n\" > \"$state_file\"\n" +
		"echo \"invalid_api_key\" 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	p := NewCommandCLI("private", script, []string{stateFile}, config.CustomPromptModeLegacy)
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
	dir := t.TempDir()
	script := filepath.Join(dir, "probe-ok.sh")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  echo ok\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	p := NewClaudeCLI(script)
	if !p.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true for healthy probe")
	}
}

func TestCLIProvider_IsAvailable_FailsWhenProbeHangs(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "probe-hang.sh")
	body := "#!/bin/sh\n" +
		"sleep 10\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

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
	dir := t.TempDir()
	script := filepath.Join(dir, "probe-hang-gemini.sh")
	body := "#!/bin/sh\n" +
		"sleep 10\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

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
	dir := t.TempDir()
	script := filepath.Join(dir, "plain-text-cli.sh")
	body := "#!/bin/sh\n" +
		"echo 'Hello from plain text'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

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
	dir := t.TempDir()
	script := filepath.Join(dir, "json-cli.sh")
	body := "#!/bin/sh\n" +
		`printf '{"result":"JSON hello","total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":5},"modelUsage":{"test-model":{"inputTokens":10,"outputTokens":5,"costUSD":0.05}}}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

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
