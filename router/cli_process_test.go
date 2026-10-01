package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
)

func TestCLIProcessChild(t *testing.T) {
	mode := os.Getenv("MAKEWAND_CLI_PROCESS_CHILD")
	if mode == "" {
		return
	}
	marker := os.Getenv("MAKEWAND_CLI_PROCESS_MARKER")
	if mode == "marker" {
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker+".ready", []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(2 * time.Second)
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker, []byte("escaped"), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if mode == "quiet" || mode == "leader" {
		// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
		child := exec.Command(os.Args[0], "-test.run=^TestCLIProcessChild$")
		child.Env = append(os.Environ(), "MAKEWAND_CLI_PROCESS_CHILD=marker")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if runtime.GOOS == "windows" {
			setCLIProcessGroup(child) // A new Windows group still belongs to the Job.
		}
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			// #nosec G703 -- marker is in the parent-created temporary fixture directory.
			if _, err := os.Stat(marker + ".ready"); err == nil {
				if mode == "quiet" {
					time.Sleep(10 * time.Second)
				} else {
					fmt.Print("leader\n")
				}
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(5)
	}
	if mode == "long-line" {
		if _, err := os.Stdout.Write(bytes.Repeat([]byte("x"), 2<<20)); err != nil {
			os.Exit(6)
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	block := append(bytes.Repeat([]byte("x"), 4095), '\n')
	size := 6 << 20
	if mode == "healthy" {
		size = 256 << 10
	}
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		for written := 0; written < size; written += len(block) {
			if _, err := stream.Write(block); err != nil {
				os.Exit(6)
			}
		}
	}
	if mode == "healthy" {
		fmt.Println("tail")
	}
	os.Exit(0)
}

func TestCLIProcessOutputOverflowIsUnknown(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			p := cliProcessProvider(t, "flood", "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var err error
			if streaming {
				_, err = collectCLIProcessStream(ctx, p)
			} else {
				_, _, err = p.chatReservedAttempt(ctx, "fixture", "fixture", "")
			}
			if !errors.Is(err, execution.ErrUnknownOutcome) || execution.ErrorStatus(err) != execution.Unknown || ctx.Err() != nil {
				t.Fatalf("overflow was not a bounded UNKNOWN: err=%v ctx=%v", err, ctx.Err())
			}
		})
	}
}

func TestCLIProcessStreamingPreservesCompleteOutput(t *testing.T) {
	p := cliProcessProvider(t, "healthy", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	content, err := collectCLIProcessStream(ctx, p)
	if err != nil || len(content) != (256<<10)+5 || !strings.HasSuffix(content, "tail\n") {
		t.Fatalf("concurrent Wait lost output: bytes=%d err=%v", len(content), err)
	}
}

func TestCLIProcessHealthProbeRejectsOutputOverflow(t *testing.T) {
	p := cliProcessProvider(t, "flood", "")
	p.binPath = os.Args[0]
	p.checkCmd = func(ctx context.Context) *exec.Cmd { return p.buildCmd(ctx, "") }
	if p.healthCheck() {
		t.Fatal("an output-flooding health probe was marked available")
	}
}

func TestCLIProcessOversizedLineCannotBlockWait(t *testing.T) {
	p := cliProcessProvider(t, "long-line", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	_, err := collectCLIProcessStream(ctx, p)
	if !errors.Is(err, execution.ErrUnknownOutcome) || ctx.Err() != nil || time.Since(started) > time.Second {
		t.Fatalf("scanner failure blocked process cleanup: err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestCLIProcessStreamingQuietDeadlineAndLeaderExit(t *testing.T) {
	for _, mode := range []string{"quiet", "leader"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "descendant")
			p := cliProcessProvider(t, mode, marker)
			timeout := 5 * time.Second
			if mode == "quiet" {
				timeout = 500 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			started := time.Now()
			_, err := collectCLIProcessStream(ctx, p)
			if time.Since(started) > 1800*time.Millisecond {
				t.Fatalf("inherited pipes exceeded wait bound: %v", err)
			}
			if mode == "quiet" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("silent stream lost its deadline: %v", err)
			}
			if mode == "leader" && err != nil && !errors.Is(err, execution.ErrUnknownOutcome) {
				t.Fatalf("incomplete descendant stdio was not UNKNOWN: %v", err)
			}
			if _, err := os.Stat(marker + ".ready"); err != nil {
				t.Fatalf("real descendant was not started: %v", err)
			}
			remaining := time.Until(started.Add(2300 * time.Millisecond))
			if remaining > 0 {
				time.Sleep(remaining)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("descendant survived command completion: %v", err)
			}
		})
	}
}

func cliProcessProvider(t *testing.T, mode, marker string) *CLIProvider {
	t.Helper()
	return &CLIProvider{provider: "offline-fixture", buildCmd: func(ctx context.Context, _ string) *exec.Cmd {
		// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIProcessChild$")
		cmd.Env = append(os.Environ(), "MAKEWAND_CLI_PROCESS_CHILD="+mode, "MAKEWAND_CLI_PROCESS_MARKER="+marker, "GORACE=atexit_sleep_ms=0")
		return cmd
	}}
}

func collectCLIProcessStream(ctx context.Context, p *CLIProvider) (string, error) {
	stream, err := p.chatStreamUnaccounted(ctx, []Message{{Role: "user", Content: "fixture"}}, "", 0)
	if err != nil {
		return "", err
	}
	var content strings.Builder
	var streamErr error
	for chunk := range stream {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		if content.Len()+len(chunk.Content) <= processjob.MaxOutputBytes+4096 {
			content.WriteString(chunk.Content)
		}
	}
	return content.String(), streamErr
}
