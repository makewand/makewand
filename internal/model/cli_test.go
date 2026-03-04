package model

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

	p := NewCommandCLI("private", script, []string{"--prompt", "{{prompt}}"})
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

	p := NewCommandCLI("private", script, []string{"--flag"})
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

	p := NewCommandCLI("private", script, []string{stateFile})
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

	p := NewCommandCLI("private", script, []string{stateFile})
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
