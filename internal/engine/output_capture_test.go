package engine

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
)

func TestOutputCapturePreservesWriterContract(t *testing.T) {
	for _, limit := range []int{0, 3, 4, 8} {
		capture := processjob.NewCapture(limit, nil)
		w := capture.Stdout()
		n, err := io.Copy(w, strings.NewReader("four"))
		if err != nil || n != 4 {
			t.Fatalf("limit %d: copied %d, error %v", limit, n, err)
		}
		if n, err := w.Write([]byte("more")); n != 4 || err != nil {
			t.Fatalf("limit %d: second write %d, %v", limit, n, err)
		}
		dropped, _ := capture.DroppedBytes()
		if len(capture.StdoutString()) != limit || dropped != int64(8-limit) {
			t.Fatalf("limit %d: retained %d, dropped %d", limit, len(capture.StdoutString()), dropped)
		}
	}
}

func TestOutputCaptureRejectsTruncatedSuccessfulCommand(t *testing.T) {
	_, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	p := &Project{Path: t.TempDir()}
	result, err := p.Exec(context.Background(), "python3", "-I", "-c", "import sys; sys.stdout.write('x' * (10 * 1024 * 1024 + 1))")
	if !errors.Is(err, execution.ErrUnknownOutcome) || result == nil || result.ExitCode != -1 {
		t.Fatalf("truncated command was treated as completed: err=%v", err)
	}
	if len(result.Stdout) != maxOutputBytes || result.StdoutDroppedBytes != 1 {
		t.Fatalf("retained %d, dropped %d", len(result.Stdout), result.StdoutDroppedBytes)
	}
}
