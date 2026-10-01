package processjob

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessCaptureChild(t *testing.T) {
	mode := os.Getenv("MAKEWAND_PROCESS_CAPTURE_CHILD")
	if mode == "" {
		return
	}
	size := MaxOutputBytes / 2
	if mode == "overflow" {
		size += 1 << 20
	}
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
	os.Exit(0)
}

func TestProcessCaptureCombinedLimit(t *testing.T) {
	for _, mode := range []string{"boundary", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessCaptureChild$")
			cmd.Env = append(os.Environ(), "MAKEWAND_PROCESS_CAPTURE_CHILD="+mode, "GORACE=atexit_sleep_ms=0")
			capture := NewCapture(MaxOutputBytes, func() { _ = Kill(cmd) })
			cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
			cleanup, err := Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			started := time.Now()
			waitErr := cmd.Wait()
			if time.Since(started) > 4*time.Second || ctx.Err() != nil {
				t.Fatalf("output supervision exceeded its bound: %v", waitErr)
			}
			retained := len(capture.StdoutString()) + len(capture.StderrString())
			stdoutDropped, stderrDropped := capture.DroppedBytes()
			if retained != MaxOutputBytes {
				t.Fatalf("combined captured bytes = %d, want %d", retained, MaxOutputBytes)
			}
			if mode == "overflow" {
				if !capture.Exceeded() || stdoutDropped+stderrDropped == 0 || waitErr == nil {
					t.Fatalf("overflow was not terminated: exceeded=%v dropped=%d wait=%v", capture.Exceeded(), stdoutDropped+stderrDropped, waitErr)
				}
			} else if capture.Exceeded() || stdoutDropped+stderrDropped != 0 || waitErr != nil {
				t.Fatalf("exact boundary failed: exceeded=%v dropped=%d wait=%v", capture.Exceeded(), stdoutDropped+stderrDropped, waitErr)
			}
		})
	}
}
