//go:build windows

package execution

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/processjob"
)

func TestVerifiedWindowsJobOwnerHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_LEASE_JOB_OWNER") != "1" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsLeaseProcessHelper$")
	cmd.Env = append(os.Environ(), "MAKEWAND_LEASE_WINDOWS_CHILD=1")
	cleanup, err := processjob.StartSandboxed(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ref := leaseProcessRef(cmd.Process.Pid)
	ref["job"] = "owner-kill-on-close"
	if err := json.NewEncoder(os.Stdout).Encode(ref); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func TestVerifiedWindowsKilledOwnerReleasesRealJobLease(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestVerifiedWindowsJobOwnerHelper$")
	cmd.Env = append(os.Environ(), "MAKEWAND_LEASE_JOB_OWNER=1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var worker map[string]any
	decoder := json.NewDecoder(pipe)
	decoder.UseNumber()
	if err := decoder.Decode(&worker); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"leases": map[string]any{"old": map[string]any{
		"keys": leaseKeys("codex", nil), "owner": leaseProcessRef(cmd.Process.Pid),
		"workers": []any{worker}, "launch_pending": false, "expires_at": json.Number("0"),
	}}}
	if _, err := admitLease(data, "codex", nil); !errors.Is(err, ErrConcurrencyBusy) {
		t.Fatalf("live Job owner lost its lease: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := admitLease(data, "codex", nil); err != nil {
		t.Fatalf("dead Job owner retained its lease: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for leaseProcessAlive(worker, false) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaseProcessAlive(worker, false) {
		t.Fatal("KILL_ON_JOB_CLOSE did not terminate the worker")
	}
}

func TestVerifiedWindowsOwnerJobCrashReclaimed(t *testing.T) {
	owner := leaseProcessRef(os.Getpid())
	owner["token"] = owner["token"].(string) + "0"
	worker := leaseProcessRef(os.Getpid())
	worker["job"] = "owner-kill-on-close"
	data := map[string]any{"leases": map[string]any{"old": map[string]any{
		"keys": leaseKeys("codex", nil), "owner": owner, "workers": []any{worker},
		"launch_pending": false, "expires_at": json.Number("0"),
	}}}
	if _, err := admitLease(data, "codex", nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsLeaseProcessHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_LEASE_WINDOWS_CHILD") == "1" {
		time.Sleep(30 * time.Second)
	}
}

func TestWindowsLeaseProcessExitAndReuse(t *testing.T) {
	ref := leaseProcessRef(os.Getpid())
	if ref["token"] == nil || !leaseProcessAlive(ref, false) {
		t.Fatalf("current process identity unavailable: %v", ref)
	}
	ref["token"] = ref["token"].(string) + "0"
	if leaseProcessAlive(ref, false) {
		t.Fatal("reused PID accepted as old owner")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsLeaseProcessHelper$")
	cmd.Env = append(os.Environ(), "MAKEWAND_LEASE_WINDOWS_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ref = leaseProcessRef(cmd.Process.Pid)
	if ref["token"] == nil || !leaseProcessAlive(ref, false) {
		t.Fatal("live child not identified")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if leaseProcessAlive(ref, false) {
		t.Fatal("exited owner still blocks reclamation")
	}
	if !leaseProcessAlive(ref, true) {
		t.Fatal("dead leader cannot prove Job descendants stopped")
	}
}
