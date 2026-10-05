package processjob

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessCaptureChild(t *testing.T) {
	mode := os.Getenv("MAKEWAND_PROCESS_CAPTURE_CHILD")
	if mode == "" {
		return
	}
	size := MaxOutputBytes / 2
	block := bytes.Repeat([]byte("x"), 32<<10)
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		for written := 0; written < size; {
			n, err := stream.Write(block[:min(len(block), size-written)])
			if err != nil {
				os.Exit(3)
			}
			written += n
		}
	}
	if mode == "overflow" {
		if _, err := os.Stderr.Write([]byte("x")); err != nil {
			os.Exit(3)
		}
		// The one excess byte must cause supervision to stop a live process,
		// rather than observing a helper that has already exited successfully.
		time.Sleep(10 * time.Second)
	}
	os.Exit(0)
}

func TestProcessCaptureCombinedLimit(t *testing.T) {
	for _, mode := range []string{"boundary", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transferCtx, stopTransfer := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopTransfer()
			stopTransferCancel := context.AfterFunc(transferCtx, cancel)
			defer stopTransferCancel()
			// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessCaptureChild$")
			cmd.Env = append(os.Environ(), "MAKEWAND_PROCESS_CAPTURE_CHILD="+mode, "GORACE=atexit_sleep_ms=0")
			stopped := make(chan time.Time, 1)
			var stopCalls atomic.Int32
			capture := NewCapture(MaxOutputBytes, func() {
				stoppedAt := time.Now()
				stopCalls.Add(1)
				select {
				case stopped <- stoppedAt:
				default:
				}
				_ = Kill(cmd)
			})
			cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
			cleanup, err := Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			waited := make(chan captureTestWaitResult, 1)
			go func() { waited <- captureTestWaitResult{err: cmd.Wait(), finishedAt: time.Now()} }()
			var result captureTestWaitResult
			var stoppedAt time.Time
			if mode == "overflow" {
				select {
				case stoppedAt = <-stopped:
				case result = <-waited:
					// Wait can finish before this test goroutine observes the
					// callback, but the callback must precede pipe completion.
					select {
					case stoppedAt = <-stopped:
					default:
						t.Fatalf("overflow helper exited without capture stop: %v", result.err)
					}
				case <-transferCtx.Done():
					t.Fatal("initialization/output transfer never reached overflow within 30s")
				}
				// Detach the transfer guard: only the real stop callback's
				// timestamp defines the following 2s cleanup deadline.
				stopTransferCancel()
				stopTransfer()
				if result.finishedAt.IsZero() {
					result = waitCaptureTestCleanup(t, waited, stoppedAt)
				}
				if elapsed := result.finishedAt.Sub(stoppedAt); elapsed < 0 || elapsed > 2*time.Second {
					t.Fatalf("overflow process/pipe cleanup took %v after capture stop", elapsed)
				}
			} else {
				select {
				case result = <-waited:
				case <-transferCtx.Done():
					t.Fatal("initialization/output transfer never completed within 30s")
				}
				stopTransferCancel()
				stopTransfer()
			}
			if ctx.Err() != nil {
				t.Fatalf("capture relied on the transfer guard to terminate the process: wait=%v ctx=%v", result.err, ctx.Err())
			}
			retained := len(capture.StdoutString()) + len(capture.StderrString())
			stdoutDropped, stderrDropped := capture.DroppedBytes()
			if retained != MaxOutputBytes {
				t.Fatalf("combined captured bytes = %d, want %d", retained, MaxOutputBytes)
			}
			if mode == "overflow" {
				if !capture.Exceeded() || stdoutDropped+stderrDropped != 1 || result.err == nil || stopCalls.Load() != 1 {
					t.Fatalf("overflow was not terminated exactly once: exceeded=%v dropped=%d stops=%d wait=%v", capture.Exceeded(), stdoutDropped+stderrDropped, stopCalls.Load(), result.err)
				}
			} else if capture.Exceeded() || stdoutDropped+stderrDropped != 0 || result.err != nil || stopCalls.Load() != 0 {
				t.Fatalf("exact boundary failed: exceeded=%v dropped=%d stops=%d wait=%v", capture.Exceeded(), stdoutDropped+stderrDropped, stopCalls.Load(), result.err)
			}
		})
	}
}

type captureTestWaitResult struct {
	err        error
	finishedAt time.Time
}

func waitCaptureTestCleanup(t *testing.T, waited <-chan captureTestWaitResult, stoppedAt time.Time) captureTestWaitResult {
	t.Helper()
	timer := time.NewTimer(time.Until(stoppedAt.Add(2 * time.Second)))
	defer timer.Stop()
	select {
	case result := <-waited:
		return result
	case <-timer.C:
		// Prefer an already completed result if this goroutine itself was
		// delayed; its timestamp still has to meet the strict cleanup bound.
		select {
		case result := <-waited:
			return result
		default:
			t.Fatal("overflow process or inherited pipes remain open 2s after capture stop")
			return captureTestWaitResult{}
		}
	}
}

func TestCaptureOverflowInvokesStopOnceAcrossStreams(t *testing.T) {
	var stops atomic.Int32
	capture := NewCapture(4, func() { stops.Add(1) })
	if n, err := capture.Stdout().Write([]byte("abcd")); n != 4 || err != nil {
		t.Fatalf("boundary Write=%d, %v", n, err)
	}
	var wg sync.WaitGroup
	for _, stream := range []struct {
		writer io.Writer
		data   []byte
	}{{capture.Stdout(), []byte("fg")}, {capture.Stderr(), []byte("ehi")}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := stream.writer.Write(stream.data); n != len(stream.data) || err != nil {
				t.Errorf("overflow Write=%d, %v; want %d, nil", n, err, len(stream.data))
			}
		}()
	}
	wg.Wait()
	stdoutDropped, stderrDropped := capture.DroppedBytes()
	if capture.StdoutString() != "abcd" || capture.StderrString() != "" || stdoutDropped != 2 || stderrDropped != 3 || !capture.Exceeded() || stops.Load() != 1 {
		t.Fatalf("shared overflow contract: stdout=%q stderr=%q dropped=%d/%d exceeded=%v stops=%d", capture.StdoutString(), capture.StderrString(), stdoutDropped, stderrDropped, capture.Exceeded(), stops.Load())
	}
}
