//go:build linux

package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Final round 2 (stream-cancel-writeback), live: a provider CLI refreshes its
// token in the sandbox, prints a first line and hangs; a cancellation or a
// deadline ends the stream. The refreshed token is on the host when the
// stream has ended - directly (CLIProvider.ChatStream), through the Router,
// and in a process that exits as soon as the stream has ended.

// streamFinal2Refresh refreshes the codex token, prints a line and hangs.
const streamFinal2Refresh = `printf '%s' '` + syncFinalNew + `' > "$HOME/.codex/auth.json"; echo partial; sleep 30; echo late`

func streamFinal2Messages() []Message { return []Message{{Role: "user", Content: "hi"}} }

// streamFinal2Cancel ends a stream as each variant does: an explicit
// cancellation after the first line, or a deadline the CLI outlives.
var streamFinal2Cancels = []struct {
	name  string
	start func(parent context.Context) (context.Context, context.CancelFunc)
	early bool // cancel as soon as the first line arrives
}{
	{"cancel", context.WithCancel, true},
	{"deadline", func(parent context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(parent, 1500*time.Millisecond)
	}, false},
}

// streamFinal2Consume reads stream to its close, cancelling after the first
// line when early is set, and returns the text read.
func streamFinal2Consume(t *testing.T, stream <-chan StreamChunk, cancel context.CancelFunc, early bool) string {
	t.Helper()
	var text strings.Builder
	timeout := time.After(time.Minute)
	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return text.String()
			}
			text.WriteString(chunk.Content)
			if early && strings.Contains(text.String(), "partial") {
				cancel()
			}
		case <-timeout:
			t.Fatal("the stream did not end")
		}
	}
}

func TestCLIStreamEndedByCancellationWritesBackFirst_Final2(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for _, s := range syncFinalSessions {
		for _, c := range streamFinal2Cancels {
			t.Run(s.name+"/"+c.name, func(t *testing.T) {
				for i := 0; i < 3; i++ {
					cred := syncFinalHome(t)
					p := NewCommandCLI("codex", "/bin/sh", []string{"-c", streamFinal2Refresh}, PromptModeArg)
					ctx, cancel := c.start(s.ctx(t.TempDir()))
					stream, err := p.ChatStream(ctx, streamFinal2Messages(), "", 100)
					if err != nil {
						cancel()
						t.Fatal(err)
					}
					text := streamFinal2Consume(t, stream, cancel, c.early)
					cancel()
					if !strings.Contains(text, "partial") {
						t.Fatalf("run %d: the CLI's first line never arrived: %q", i, text)
					}
					if got := syncFinalRead(t, cred); got != syncFinalNew {
						t.Fatalf("run %d: the refreshed token was not on the host when the stream ended: %q", i, got)
					}
				}
			})
		}
	}
}

// The same through the Router, whose own stream and the dispatch wrapper end
// only once the CLI's stream has.
func TestRouterCLIStreamEndedByCancellationWritesBackFirst_Final2(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for _, c := range streamFinal2Cancels {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				cred := syncFinalHome(t)
				r := mustNewRouter(RouterConfig{
					Providers: map[string]ProviderEntry{
						"codex": {Provider: NewCommandCLI("codex", "/bin/sh", []string{"-c", streamFinal2Refresh}, PromptModeArg), Access: AccessSubscription},
					},
					DefaultModel: "codex",
					CodingModel:  "codex",
				})
				ctx, cancel := c.start(ContextWithWorkDir(context.Background(), t.TempDir()))
				stream, _, err := r.ChatStream(ctx, TaskCode, streamFinal2Messages(), "")
				if err != nil {
					cancel()
					t.Fatalf("run %d: ChatStream: %v", i, err)
				}
				text := streamFinal2Consume(t, stream, cancel, c.early)
				cancel()
				if !strings.Contains(text, "partial") {
					t.Fatalf("run %d: the CLI's first line never arrived: %q", i, text)
				}
				if got := syncFinalRead(t, cred); got != syncFinalNew {
					t.Fatalf("run %d: the refreshed token was not on the host when the Router's stream ended: %q", i, got)
				}
			}
		})
	}
}

// TestStreamFinal2ExitHelper is the process of
// TestCLIStreamCancelledRefreshSurvivesProcessExit_Final2: it streams from
// the refreshing CLI, cancels after the first line and exits as soon as the
// stream has ended.
func TestStreamFinal2ExitHelper(t *testing.T) {
	mode := os.Getenv("MAKEWAND_STREAM_FINAL2_EXIT")
	if mode == "" {
		return
	}
	p := NewCommandCLI("codex", "/bin/sh", []string{"-c", streamFinal2Refresh}, PromptModeArg)
	ctx, cancel := context.WithCancel(ContextWithWorkDir(context.Background(), os.Getenv("MAKEWAND_STREAM_FINAL2_WS")))
	var stream <-chan StreamChunk
	var err error
	if mode == "router" {
		r := mustNewRouter(RouterConfig{
			Providers:    map[string]ProviderEntry{"codex": {Provider: p, Access: AccessSubscription}},
			DefaultModel: "codex",
			CodingModel:  "codex",
		})
		stream, _, err = r.ChatStream(ctx, TaskCode, streamFinal2Messages(), "")
	} else {
		stream, err = p.ChatStream(ctx, streamFinal2Messages(), "", 100)
	}
	if err != nil {
		os.Exit(3)
	}
	for chunk := range stream {
		if strings.Contains(chunk.Content, "partial") {
			cancel()
		}
	}
	if cliTestPrivateHomeBase != "" {
		_ = os.RemoveAll(cliTestPrivateHomeBase) // TestMain's cleanup, which os.Exit skips
	}
	os.Exit(0) // the process exits as soon as the stream has ended
}

// A process that exits as soon as a cancelled stream has ended keeps the
// token its provider refreshed in the sandbox.
func TestCLIStreamCancelledRefreshSurvivesProcessExit_Final2(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	for _, mode := range []string{"provider", "router"} {
		t.Run(mode, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				home := t.TempDir()
				cred := filepath.Join(home, ".codex", "auth.json")
				readonlyFixWrite(t, cred, syncFinalOld)
				// #nosec G204 G702 -- the trusted Go test executable with a fixed selector.
				cmd := exec.Command(os.Args[0], "-test.run=^TestStreamFinal2ExitHelper$", "-test.count=1")
				cmd.Env = append(os.Environ(), "HOME="+home, "MAKEWAND_STREAM_FINAL2_EXIT="+mode, "MAKEWAND_STREAM_FINAL2_WS="+t.TempDir(), "GORACE=atexit_sleep_ms=0")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("run %d: helper: %v\n%s", i, err, out)
				}
				if got := syncFinalRead(t, cred); got != syncFinalNew {
					t.Fatalf("run %d: the refreshed token was lost when the process exited after the stream: %q", i, got)
				}
			}
		})
	}
}
