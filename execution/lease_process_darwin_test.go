package execution

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestDarwinLeaseStartTimeAndReuse(t *testing.T) {
	ref := leaseProcessRef(os.Getpid())
	if ref["token"] == nil || !leaseProcessAlive(ref, false) {
		t.Fatalf("current process start time unavailable: %v", ref)
	}
	ref["token"] = "different process start"
	if leaseProcessAlive(ref, false) {
		t.Fatal("reused PID accepted as original owner")
	}
	ref["token"] = nil
	if !leaseProcessAlive(ref, false) {
		t.Fatal("missing start time must conservatively retain a live PID")
	}
}

func TestDarwinLeaseProcessHelper(t *testing.T) {
	if os.Getenv("MAKEWAND_LEASE_DARWIN_CHILD") == "1" {
		time.Sleep(30 * time.Second)
	}
}

func TestDarwinLeaseProcessExit(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDarwinLeaseProcessHelper$")
	cmd.Env = append(os.Environ(), "MAKEWAND_LEASE_DARWIN_CHILD=1")
	setLeaseFixtureGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ref := leaseProcessRef(cmd.Process.Pid)
	if ref["token"] == nil || !leaseProcessAlive(ref, false) {
		t.Fatalf("live child identity unavailable: %v", ref)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if leaseProcessAlive(ref, false) || leaseProcessAlive(ref, true) {
		t.Fatal("joined child without descendants still blocks reclamation")
	}
}
