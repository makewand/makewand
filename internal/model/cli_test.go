package model

import (
	"context"
	"os"
	"path/filepath"
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
