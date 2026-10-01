package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/makewand/makewand/execution"
)

func TestEngineProcessChild(t *testing.T) {
	mode := os.Getenv("MAKEWAND_ENGINE_PROCESS_CHILD")
	if mode == "" {
		return
	}
	marker := os.Getenv("MAKEWAND_ENGINE_PROCESS_MARKER")
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
	if mode == "leader" || mode == "preview" || mode == "preview-leader" {
		// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
		child := exec.Command(os.Args[0], "-test.run=^TestEngineProcessChild$")
		child.Env = append(os.Environ(), "MAKEWAND_ENGINE_PROCESS_CHILD=marker")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if runtime.GOOS == "windows" {
			setProcessGroup(child)
		}
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			// #nosec G703 -- marker is in the parent-created temporary fixture directory.
			if _, err := os.Stat(marker + ".ready"); err == nil {
				fmt.Print("leader\n")
				if mode == "preview" {
					time.Sleep(10 * time.Second)
				}
				if mode == "preview-leader" {
					time.Sleep(100 * time.Millisecond)
				}
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(5)
	}
	block := bytes.Repeat([]byte("x"), 32<<10)
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		for written := 0; written < 6<<20; written += len(block) {
			if _, err := stream.Write(block); err != nil {
				os.Exit(6)
			}
		}
	}
	os.Exit(0)
}

func TestPreviewProcessOverflowPublishesUnknown(t *testing.T) {
	configurePreviewProcessFixture(t, "flood", "", false)
	p := &Project{Path: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, err := p.StartPreview(ctx, false)
	if err != nil {
		if !errors.Is(err, execution.ErrUnknownOutcome) {
			t.Fatalf("early preview overflow lost UNKNOWN: %v", err)
		}
		return
	}
	defer server.Stop()
	select {
	case <-server.Done():
		if !errors.Is(server.Err(), execution.ErrUnknownOutcome) {
			t.Fatalf("preview overflow was not UNKNOWN: %v", server.Err())
		}
	case <-ctx.Done():
		t.Fatal("preview overflow failed to stop")
	}
}

func TestPreviewProcessStopAndLeaderExitClearDescendants(t *testing.T) {
	for _, mode := range []string{"preview", "preview-leader"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "descendant")
			configurePreviewProcessFixture(t, mode, marker, true)
			p := &Project{Path: t.TempDir()}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := time.Now()
			server, err := p.StartPreview(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Stop()
			if mode == "preview" {
				stopped := make(chan struct{})
				go func() { server.Stop(); close(stopped) }()
				server.Stop()
				<-stopped
			} else {
				select {
				case <-server.Done():
				case <-ctx.Done():
					t.Fatal("exited preview leader was not reaped")
				}
				if err := server.Err(); err != nil && !errors.Is(err, execution.ErrUnknownOutcome) {
					t.Fatalf("inherited preview pipes lost UNKNOWN: %v", err)
				}
			}
			if time.Since(started) > 1800*time.Millisecond {
				t.Fatalf("preview stop/exit exceeded bound: %v", time.Since(started))
			}
			if remaining := time.Until(started.Add(2300 * time.Millisecond)); remaining > 0 {
				time.Sleep(remaining)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("preview descendant survived: %v", err)
			}
		})
	}
}

func configurePreviewProcessFixture(t *testing.T, mode, marker string, waitReady bool) {
	t.Helper()
	oldCommand, oldPort, oldWait := previewCommandContext, previewFindFreePort, previewWaitForPort
	t.Cleanup(func() {
		previewCommandContext, previewFindFreePort, previewWaitForPort = oldCommand, oldPort, oldWait
	})
	t.Setenv("MAKEWAND_ENGINE_PROCESS_CHILD", mode)
	t.Setenv("MAKEWAND_ENGINE_PROCESS_MARKER", marker)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	previewCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEngineProcessChild$")
	}
	previewFindFreePort = func() (int, error) { return 54321, nil }
	previewWaitForPort = func(ctx context.Context, _ int, _ time.Duration) error {
		if !waitReady {
			return nil
		}
		for {
			if _, err := os.Stat(marker + ".ready"); err == nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
}

func TestEngineProcessCombinedOutputOverflowIsUnknown(t *testing.T) {
	t.Setenv("MAKEWAND_ENGINE_PROCESS_CHILD", "flood")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	p := &Project{Path: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := p.execWithPolicy(ctx, os.Args[0], []string{"-test.run=^TestEngineProcessChild$"}, execPolicy{allowAnyCommand: true, allowCommandPath: true})
	if !errors.Is(err, execution.ErrUnknownOutcome) || result == nil || result.ExitCode != -1 || ctx.Err() != nil {
		t.Fatalf("output overflow was not bounded UNKNOWN: err=%v ctx=%v", err, ctx.Err())
	}
	if len(result.Stdout)+len(result.Stderr) != maxOutputBytes || result.StdoutDroppedBytes+result.StderrDroppedBytes == 0 {
		t.Fatalf("combined capture limit failed: stdout=%d stderr=%d dropped=%d", len(result.Stdout), len(result.Stderr), result.StdoutDroppedBytes+result.StderrDroppedBytes)
	}
}

func TestEngineProcessLeaderExitClearsDescendantsAndPipes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant")
	t.Setenv("MAKEWAND_ENGINE_PROCESS_CHILD", "leader")
	t.Setenv("MAKEWAND_ENGINE_PROCESS_MARKER", marker)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	p := &Project{Path: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	_, err := p.execWithPolicy(ctx, os.Args[0], []string{"-test.run=^TestEngineProcessChild$"}, execPolicy{allowAnyCommand: true, allowCommandPath: true})
	if time.Since(started) > 1800*time.Millisecond || (err != nil && !errors.Is(err, execution.ErrUnknownOutcome)) {
		t.Fatalf("leader/stdio was not bounded: err=%v elapsed=%v", err, time.Since(started))
	}
	if _, err := os.Stat(marker + ".ready"); err != nil {
		t.Fatalf("real descendant was not started: %v", err)
	}
	if remaining := time.Until(started.Add(2300 * time.Millisecond)); remaining > 0 {
		time.Sleep(remaining)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived leader completion: %v", err)
	}
}
