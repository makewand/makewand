//go:build !windows

package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNonRegularLedgerLockAndEventsFailWithoutBlocking(t *testing.T) {
	for _, target := range []string{"ledger", "lock", "events"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "calls.json")
			fifo := path
			if target == "lock" {
				fifo += ".lock"
			}
			if target == "events" {
				fifo = filepath.Join(dir, "events.jsonl")
			}
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cfg := Config{TaskID: "fifo-test", LedgerPath: path, MaxCalls: 1}
			var warnings int
			if target == "events" {
				cfg.LedgerPath = ""
				cfg.MaxCalls = 0
				cfg.EventsPath = fifo
				cfg.EventError = func(error) { warnings++ }
			}
			ctx = ContextWithConfig(ctx, cfg)
			result := make(chan error, 1)
			go func() { _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); result <- err }()
			select {
			case err := <-result:
				if target == "events" {
					if err != nil || warnings != 1 {
						t.Fatalf("optional FIFO recorder changed task: err=%v warnings=%d", err, warnings)
					}
				} else if err == nil {
					t.Fatal("nonregular budget accepted")
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("FIFO bypassed bounded admission/telemetry")
			}
		})
	}
}

func TestAdmissionLockRespectsCallerDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	lock, err := openLockFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if locked, err := tryLock(lock); err != nil || !locked {
		t.Fatalf("fixture lock: %v", err)
	}
	defer func() { _ = unlock(lock) }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	ctx = ContextWithConfig(ctx, Config{LedgerPath: path, MaxCalls: 1, TaskID: "deadline-test"})
	start := time.Now()
	if _, err := Reserve(ctx, Metadata{Engine: "synthetic", Tier: "standard"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("admission deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("admission waited %v", elapsed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("non-dispatched deadline created attempt: %v", err)
	}
}
