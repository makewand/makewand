//go:build windows

package processjob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These run the Go test binary itself: real CreateProcess, suspended job
// assignment, new child groups and inherited standard handles, with no vendor
// executable, script interpreter or paid call involved.
func TestWindowsProcessJobChild(t *testing.T) {
	mode := os.Getenv("MAKEWAND_WINDOWS_JOB_CHILD")
	if mode == "" {
		return
	}
	marker := os.Getenv("MAKEWAND_WINDOWS_JOB_MARKER")
	if mode == "marker" || mode == "armed-marker" {
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker+".ready", []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		if mode == "armed-marker" {
			// The escape timer belongs to the running phase, after the parent
			// has observed both the descendant and inherited stdout readiness.
			deadline := time.Now().Add(30 * time.Second)
			for {
				// #nosec G703 -- parent-owned fixture gate in the temporary directory.
				if _, err := os.Stat(marker + ".armed"); err == nil {
					break
				}
				if time.Now().After(deadline) {
					os.Exit(7)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		time.Sleep(900 * time.Millisecond)
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if err := os.WriteFile(marker, []byte("escaped"), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if mode == "hold" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	if mode == "initialization-delay" {
		// The real context deadline must terminate the process even when
		// initialization has not reached descendant creation or stdout.
		time.Sleep(10 * time.Second)
		os.Exit(7)
	}
	if delay := os.Getenv("MAKEWAND_WINDOWS_JOB_STARTUP_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			os.Exit(8)
		}
		time.Sleep(duration)
	}
	// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
	child := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessJobChild$")
	childMode := "marker"
	if mode == "timeout" {
		childMode = "armed-marker"
	}
	child.Env = append(os.Environ(), "MAKEWAND_WINDOWS_JOB_CHILD="+childMode)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if mode == "breakaway" {
		child.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	if err := child.Start(); err != nil {
		if mode == "breakaway" || mode == "process-limit" {
			fmt.Print("blocked")
			os.Exit(0)
		}
		fmt.Fprint(os.Stderr, err)
		os.Exit(4)
	}
	if mode == "breakaway" || mode == "process-limit" {
		_ = child.Process.Kill()
		_ = child.Wait()
		os.Exit(5)
	}
	deadline := time.Now().Add(3 * time.Second)
	if mode == "timeout" {
		deadline = time.Now().Add(30 * time.Second)
	}
	for time.Now().Before(deadline) {
		// #nosec G703 -- marker is in the parent-created temporary fixture directory.
		if _, err := os.Stat(marker + ".ready"); err == nil {
			fmt.Print("spawned")
			if mode == "timeout" {
				time.Sleep(10 * time.Second)
			}
			os.Exit(0)
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.Exit(6)
}

func TestWindowsProcessJobLeaderExitKillsNewGroupAndPipes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := windowsJobCommand(ctx, "leader", marker)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cleanup, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	started := time.Now()
	if err := cmd.Wait(); err != nil || output.String() != "spawned" || time.Since(started) > 2*time.Second {
		t.Fatalf("leader/stdio did not finish cleanly: output=%q err=%v", output.String(), err)
	}
	assertWindowsJobMarkerAbsent(t, marker)
}

func TestWindowsProcessJobDeadlineBeforeReadinessKillsProcessAndPipes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant")
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	cmd := windowsJobCommand(ctx, "initialization-delay", marker)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cleanup, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	started := time.Now()
	if err := cmd.Wait(); err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || output.Len() != 0 || cmd.ProcessState == nil || !cmd.ProcessState.Exited() || time.Since(started) > 2*time.Second {
		t.Fatalf("deadline failed before readiness: output=%q err=%v ctx=%v state=%v", output.String(), err, ctx.Err(), cmd.ProcessState)
	}
	if _, err := os.Stat(marker + ".ready"); !os.IsNotExist(err) {
		t.Fatalf("initialization-delay fixture reached descendant readiness: %v", err)
	}
	assertWindowsJobMarkerAbsent(t, marker)
}

func TestWindowsProcessJobCancellationAfterReadinessKillsNewGroupAndPipes(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second} {
		t.Run("startup-delay-"+delay.String(), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "descendant")
			startupCtx, stopStartup := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopStartup()
			ctx, cancel := context.WithCancel(startupCtx)
			defer cancel()
			cmd := windowsJobCommand(ctx, "timeout", marker)
			cmd.Env = append(cmd.Env, "MAKEWAND_WINDOWS_JOB_STARTUP_DELAY="+delay.String())
			output := &windowsJobReadinessOutput{ready: make(chan struct{})}
			cmd.Stdout, cmd.Stderr = output, output
			cleanup, err := Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			select {
			case <-output.ready:
			case err := <-waited:
				t.Fatalf("job exited before readiness: output=%q err=%v", output.String(), err)
			case <-startupCtx.Done():
				t.Fatalf("job never became ready: output=%q ctx=%v", output.String(), startupCtx.Err())
			}
			if _, err := os.Stat(marker + ".ready"); err != nil {
				t.Fatalf("stdout readiness did not include a ready descendant: %v", err)
			}
			// Keep the original 700ms running window. Startup has a separate
			// bounded guard, and this contract deliberately expects Canceled.
			started := time.Now()
			timer := time.AfterFunc(700*time.Millisecond, cancel)
			defer timer.Stop()
			// #nosec G703 -- parent-owned fixture gate in the temporary directory.
			if err := os.WriteFile(marker+".armed", []byte("armed"), 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waited:
				if err == nil || !errors.Is(ctx.Err(), context.Canceled) || output.String() != "spawned" || cmd.ProcessState == nil || !cmd.ProcessState.Exited() || time.Since(started) > 2*time.Second {
					t.Fatalf("cancellation failed after readiness: output=%q err=%v ctx=%v state=%v", output.String(), err, ctx.Err(), cmd.ProcessState)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("ready job or inherited pipes did not close after cancellation: output=%q ctx=%v", output.String(), ctx.Err())
			}
			assertWindowsJobMarkerAbsent(t, marker)
		})
	}
}

type windowsJobReadinessOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (w *windowsJobReadinessOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if bytes.Contains(w.buffer.Bytes(), []byte("spawned")) {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func (w *windowsJobReadinessOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func TestWindowsProcessJobRefusesBreakawayAndEnforcesProcessLimit(t *testing.T) {
	for _, mode := range []string{"breakaway", "process-limit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			cmd := windowsJobCommand(ctx, mode, filepath.Join(t.TempDir(), "descendant"))
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			maximum := uint32(1)
			if mode == "breakaway" {
				maximum = 3
			}
			cleanup, err := StartWithOptions(cmd, Options{MaxProcesses: maximum})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if err := cmd.Wait(); err != nil || strings.TrimSpace(output.String()) != "blocked" {
				t.Fatalf("child escaped required Job policy: output=%q err=%v", output.String(), err)
			}
		})
	}
}

func TestWindowsProcessJobMemoryLimitConfigured(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := windowsJobCommand(ctx, "hold", "")
	const memory = uintptr(512 << 20)
	cleanup, err := StartWithOptions(cmd, Options{MaxProcesses: 3, MemoryBytes: memory})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer func() { _ = cmd.Wait() }()
	defer func() { _ = Kill(cmd) }()
	value, ok := jobs.Load(cmd)
	if !ok {
		t.Fatal("started process has no owned job")
	}
	lease := value.(*jobLease)
	lease.mu.Lock()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err = windows.QueryInformationJobObject(lease.handle, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), nil)
	lease.mu.Unlock()
	if err != nil || limits.JobMemoryLimit != memory || limits.BasicLimitInformation.ActiveProcessLimit != 3 || limits.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_JOB_MEMORY == 0 {
		t.Fatalf("required resource limits are absent: limits=%+v err=%v", limits, err)
	}
}

func windowsJobCommand(ctx context.Context, mode, marker string) *exec.Cmd {
	// #nosec G204 G702 -- parent-controlled Go test executable and fixed helper selector.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsProcessJobChild$")
	cmd.Env = append(os.Environ(), "MAKEWAND_WINDOWS_JOB_CHILD="+mode, "MAKEWAND_WINDOWS_JOB_MARKER="+marker, "GORACE=atexit_sleep_ms=0")
	return cmd
}

func assertWindowsJobMarkerAbsent(t *testing.T, marker string) {
	t.Helper()
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived supervised leader: marker err=%v", err)
	}
}
