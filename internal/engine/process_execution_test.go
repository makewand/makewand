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
		if !waitEngineProcessChildMarker(marker, ".armed") {
			os.Exit(7)
		}
		// The escape window belongs to the real running phase after startup.
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker+".armed-ready", []byte("armed"), 0o600); err != nil {
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
		if !waitEngineProcessChildMarker(marker, ".ready") {
			os.Exit(5)
		}
		if n, err := fmt.Print("leader\n"); n != len("leader\n") || err != nil {
			os.Exit(6)
		}
		// This acknowledgment follows the successful write to the real pipe.
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker+".leader-output", []byte("leader\n"), 0o600); err != nil {
			os.Exit(3)
		}
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker+".leader-ready", []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		if !waitEngineProcessChildMarker(marker, ".armed-ready") {
			os.Exit(7)
		}
		if mode == "preview" {
			time.Sleep(10 * time.Second)
		} else {
			if !waitEngineProcessChildMarker(marker, ".exit") {
				os.Exit(7)
			}
			if mode == "preview-leader" {
				time.Sleep(100 * time.Millisecond)
			}
			// #nosec G703 -- marker records the real leader's final exit path.
			if err := os.WriteFile(marker+".leader-exit", []byte("0"), 0o600); err != nil {
				os.Exit(3)
			}
		}
		os.Exit(0)
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

func waitEngineProcessChildMarker(marker, suffix string) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if _, err := os.Stat(marker + suffix); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func waitEngineProcessFixtureMarker(t *testing.T, ctx context.Context, marker, suffix string, done <-chan struct{}) {
	t.Helper()
	for {
		if _, err := os.Stat(marker + suffix); err == nil {
			return
		}
		select {
		case <-done:
			t.Fatalf("owned process exited before real fixture readiness %s", suffix)
		case <-ctx.Done():
			t.Fatalf("fixture never reached %s within startup guard: %v", suffix, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func armEngineProcessFixture(t *testing.T, ctx context.Context, marker string, done <-chan struct{}) time.Time {
	t.Helper()
	if err := os.WriteFile(marker+".armed", []byte("armed"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitEngineProcessFixtureMarker(t, ctx, marker, ".armed-ready", done)
	return time.Now()
}

func assertEngineProcessDidNotEscape(t *testing.T, marker string, armedReadyAt time.Time) {
	t.Helper()
	// The descendant began its real two-second timer before this acknowledgment
	// was observed, so this probe cannot succeed merely because startup was slow.
	if remaining := time.Until(armedReadyAt.Add(2300 * time.Millisecond)); remaining > 0 {
		time.Sleep(remaining)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("owned descendant survived process completion: %v", err)
	}
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			server, err := p.StartPreview(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cancel()
				stopped := make(chan struct{})
				go func() { server.Stop(); close(stopped) }()
				select {
				case <-stopped:
				case <-time.After(2 * time.Second):
					t.Error("owned preview did not finish after guard cancellation")
				}
			}()
			waitEngineProcessFixtureMarker(t, ctx, marker, ".ready", server.Done())
			armedReadyAt := armEngineProcessFixture(t, ctx, marker, server.Done())
			started := time.Now()
			var finishedAt time.Time
			timer := time.NewTimer(1800 * time.Millisecond)
			defer timer.Stop()
			if mode == "preview" {
				stopped := make(chan time.Time, 2)
				for range 2 {
					go func() { server.Stop(); stopped <- time.Now() }()
				}
				for range 2 {
					select {
					case completedAt := <-stopped:
						if completedAt.After(finishedAt) {
							finishedAt = completedAt
						}
						if completedAt.Sub(started) > 1800*time.Millisecond {
							t.Fatalf("real preview Stop exceeded bound: %v", completedAt.Sub(started))
						}
					case <-timer.C:
						t.Fatal("real preview Stop or inherited pipes exceeded 1800ms")
					}
				}
			} else {
				if err := os.WriteFile(marker+".exit", []byte("exit"), 0o600); err != nil {
					t.Fatal(err)
				}
				select {
				case <-server.Done():
					finishedAt = time.Now()
				case <-timer.C:
					t.Fatal("released preview leader or inherited pipes exceeded 1800ms")
				}
				if err := server.Err(); err != nil && !errors.Is(err, execution.ErrUnknownOutcome) {
					t.Fatalf("inherited preview pipes lost UNKNOWN: %v", err)
				}
				if server.cmd.ProcessState == nil || server.cmd.ProcessState.ExitCode() != 0 {
					t.Fatalf("real preview leader did not exit zero: %v", server.cmd.ProcessState)
				}
				data, err := os.ReadFile(marker + ".leader-exit")
				if err != nil || string(data) != "0" {
					t.Fatalf("preview leader did not reach the released exit-zero path: %q %v", data, err)
				}
			}
			if ctx.Err() != nil || finishedAt.Sub(started) > 1800*time.Millisecond {
				t.Fatalf("preview stop/exit exceeded running bound: ctx=%v elapsed=%v", ctx.Err(), finishedAt.Sub(started))
			}
			data, err := os.ReadFile(marker + ".leader-output")
			if err != nil || string(data) != "leader\n" {
				t.Fatalf("real inherited stdout write did not complete: %q %v", data, err)
			}
			assertEngineProcessDidNotEscape(t, marker, armedReadyAt)
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
			if _, err := os.Stat(marker + ".leader-ready"); err == nil {
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

func TestEngineProcessStartFailureHonorsContext(t *testing.T) {
	for _, mode := range []string{"canceled", "deadline", "missing"} {
		t.Run(mode, func(t *testing.T) {
			p := &Project{Path: t.TempDir()}
			missing := filepath.Join(p.Path, "missing-fixture.exe")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var contextErr error
			switch mode {
			case "canceled":
				cancel()
				contextErr = context.Canceled
			case "deadline":
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				contextErr = context.DeadlineExceeded
			}
			// execWithPolicy uses a real exec.Cmd: this absent private path
			// reaches Start and cannot dispatch any tool or child process.
			result, err := p.execWithPolicy(ctx, missing, nil, execPolicy{allowAnyCommand: true, allowCommandPath: true})
			if result != nil || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixture did not reach the actual missing-executable Start: result=%+v err=%v", result, err)
			}
			if contextErr != nil {
				if !errors.Is(err, contextErr) || !errors.Is(err, execution.ErrUnknownOutcome) {
					t.Fatalf("interrupted Start lost context or conservative unknown: %v", err)
				}
			} else if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, execution.ErrUnknownOutcome) {
				t.Fatalf("ordinary missing executable became interruption: %v", err)
			}
		})
	}
}

func TestEngineProcessLeaderExitClearsDescendantsAndPipes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant")
	t.Setenv("MAKEWAND_ENGINE_PROCESS_CHILD", "leader")
	t.Setenv("MAKEWAND_ENGINE_PROCESS_MARKER", marker)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	p := &Project{Path: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result *ExecResult
	var err error
	var completedAt time.Time
	done := make(chan struct{})
	go func() {
		result, err = p.execWithPolicy(ctx, os.Args[0], []string{"-test.run=^TestEngineProcessChild$"}, execPolicy{allowAnyCommand: true, allowCommandPath: true})
		completedAt = time.Now()
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("owned engine process did not finish after guard cancellation")
		}
	}()
	waitEngineProcessFixtureMarker(t, ctx, marker, ".leader-ready", done)
	waitEngineProcessFixtureMarker(t, ctx, marker, ".ready", done)
	armedReadyAt := armEngineProcessFixture(t, ctx, marker, done)
	started := time.Now()
	if err := os.WriteFile(marker+".exit", []byte("exit"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(1800 * time.Millisecond):
		t.Fatal("released engine leader or inherited pipes exceeded 1800ms")
	}
	if ctx.Err() != nil || completedAt.Sub(started) > 1800*time.Millisecond || (err != nil && !errors.Is(err, execution.ErrUnknownOutcome)) || result == nil || result.Stdout != "leader\n" || (err == nil && result.ExitCode != 0) {
		t.Fatalf("leader/stdio was not bounded after readiness: err=%v ctx=%v elapsed=%v result=%+v", err, ctx.Err(), completedAt.Sub(started), result)
	}
	data, readErr := os.ReadFile(marker + ".leader-exit")
	if readErr != nil || string(data) != "0" {
		t.Fatalf("engine leader did not reach the released exit-zero path: %q %v", data, readErr)
	}
	assertEngineProcessDidNotEscape(t, marker, armedReadyAt)
}
