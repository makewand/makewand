package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestStartPreview_RequiresAllowProjectScriptsForNode(t *testing.T) {
	proj, err := NewProject("preview-node", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.WriteFile("package.json", `{"name":"x","scripts":{"dev":"vite"}}`); err != nil {
		t.Fatalf("WriteFile(package.json): %v", err)
	}

	_, err = proj.StartPreview(context.Background(), false)
	if err == nil {
		t.Fatal("StartPreview should reject project scripts without explicit allow flag")
	}
	if !strings.Contains(err.Error(), "--allow-project-scripts") {
		t.Fatalf("StartPreview error = %q, want --allow-project-scripts hint", err.Error())
	}
}

func TestStartPreview_RequiresAllowProjectScriptsForPython(t *testing.T) {
	proj, err := NewProject("preview-python", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.WriteFile("manage.py", "print('ok')\n"); err != nil {
		t.Fatalf("WriteFile(manage.py): %v", err)
	}

	_, err = proj.StartPreview(context.Background(), false)
	if err == nil {
		t.Fatal("StartPreview should reject manage.py execution without explicit allow flag")
	}
	if !strings.Contains(err.Error(), "--allow-project-scripts") {
		t.Fatalf("StartPreview error = %q, want --allow-project-scripts hint", err.Error())
	}
}

func TestWaitForPreviewPort_SucceedsAfterRetries(t *testing.T) {
	oldDial := previewDialTimeout
	t.Cleanup(func() { previewDialTimeout = oldDial })

	attempts := 0
	previewDialTimeout = func(network, address string, timeout time.Duration) (net.Conn, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection refused")
		}
		return &stubConn{}, nil
	}

	if err := waitForPreviewPort(context.Background(), 12345, time.Second); err != nil {
		t.Fatalf("waitForPreviewPort() error = %v, want nil", err)
	}
	if attempts < 3 {
		t.Fatalf("attempts=%d, want at least 3 retries", attempts)
	}
}

func TestWaitForPreviewPort_TimesOut(t *testing.T) {
	oldDial := previewDialTimeout
	t.Cleanup(func() { previewDialTimeout = oldDial })

	previewDialTimeout = func(network, address string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}

	err := waitForPreviewPort(context.Background(), 12345, 220*time.Millisecond)
	if err == nil {
		t.Fatal("waitForPreviewPort() error = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("waitForPreviewPort() error = %q, want timeout detail", err.Error())
	}
}

func TestStartPreview_ReturnsErrorWhenPortNeverBecomesReady(t *testing.T) {
	oldCmd := previewCommandContext
	oldFindPort := previewFindFreePort
	oldWaitPort := previewWaitForPort
	t.Cleanup(func() {
		previewCommandContext = oldCmd
		previewFindFreePort = oldFindPort
		previewWaitForPort = oldWaitPort
	})

	proj, err := NewProject("preview-false-success", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.WriteFile("index.html", "<!doctype html><p>ok</p>"); err != nil {
		t.Fatalf("WriteFile(index.html): %v", err)
	}

	previewFindFreePort = func() (int, error) { return 43210, nil }
	previewWaitForPort = func(ctx context.Context, port int, timeout time.Duration) error {
		if port != 43210 {
			t.Fatalf("preview port=%d, want 43210", port)
		}
		return fmt.Errorf("timed out after %s: connection refused", timeout)
	}
	previewCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Long-running process that should be killed by StartPreview on readiness failure.
		return exec.CommandContext(ctx, "sh", "-c", "sleep 5")
	}

	server, err := proj.StartPreview(context.Background(), false)
	if err == nil {
		t.Fatal("StartPreview() error = nil, want readiness failure")
	}
	if server != nil {
		t.Fatal("StartPreview() server != nil on readiness failure")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("StartPreview() error = %q, want readiness failure detail", err.Error())
	}
}

func TestStartPreview_IncludesStartupStderrOnReadinessFailure(t *testing.T) {
	oldCmd := previewCommandContext
	oldFindPort := previewFindFreePort
	oldWaitPort := previewWaitForPort
	oldWrap := previewWrapProjectCmd
	t.Cleanup(func() {
		previewCommandContext = oldCmd
		previewFindFreePort = oldFindPort
		previewWaitForPort = oldWaitPort
		previewWrapProjectCmd = oldWrap
	})

	proj, err := NewProject("preview-stderr-hint", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.WriteFile("package.json", `{"name":"x","scripts":{"dev":"node dev.js"}}`); err != nil {
		t.Fatalf("WriteFile(package.json): %v", err)
	}

	previewFindFreePort = func() (int, error) { return 45678, nil }
	previewWaitForPort = func(ctx context.Context, port int, timeout time.Duration) error {
		time.Sleep(50 * time.Millisecond)
		return fmt.Errorf("timed out after %s: connection refused", timeout)
	}
	previewWrapProjectCmd = func(projectPath, command string, args []string) (string, []string, error) {
		return "sh", []string{"-c", "echo uid map denied >&2; sleep 5"}, nil
	}
	previewCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, name, args...)
	}

	server, err := proj.StartPreview(context.Background(), true)
	if err == nil {
		t.Fatal("StartPreview() error = nil, want readiness failure")
	}
	if server != nil {
		t.Fatal("StartPreview() server != nil on readiness failure")
	}
	if !strings.Contains(err.Error(), "startup stderr") {
		t.Fatalf("StartPreview() error = %q, want startup stderr detail", err.Error())
	}
	if !strings.Contains(err.Error(), "uid map denied") {
		t.Fatalf("StartPreview() error = %q, want startup stderr content", err.Error())
	}
}

type stubConn struct{}

func (stubConn) Read(b []byte) (int, error)         { return 0, nil }
func (stubConn) Write(b []byte) (int, error)        { return len(b), nil }
func (stubConn) Close() error                       { return nil }
func (stubConn) LocalAddr() net.Addr                { return stubAddr("local") }
func (stubConn) RemoteAddr() net.Addr               { return stubAddr("remote") }
func (stubConn) SetDeadline(t time.Time) error      { return nil }
func (stubConn) SetReadDeadline(t time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(t time.Time) error { return nil }

type stubAddr string

func (a stubAddr) Network() string { return "tcp" }
func (a stubAddr) String() string  { return string(a) }
