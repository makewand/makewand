package model

import (
	"context"
	"os"
	"path/filepath"
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
