package execution

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestR3LeaseWaitReservesTimeBeforeOwnDeadline(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		cap      string
		deadline time.Duration
		want     time.Duration
	}{
		{"default-bounded", "", 3 * time.Minute, 30 * time.Second},
		{"default-unbounded", "", 0, 30 * time.Second},
		{"explicit-cap", "2", 3 * time.Minute, 2 * time.Second},
		{"shorter-deadline", "80", time.Second, time.Second / 2},
		{"immediate", "0", 3 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", tc.cap)
			ctx := context.Background()
			if tc.deadline != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, now.Add(tc.deadline))
				defer cancel()
			}
			got, err := leaseWaitDeadline(ctx, now)
			if err != nil || !got.Equal(now.Add(tc.want)) {
				t.Fatalf("wait deadline = %v, %v; want %v", got, err, now.Add(tc.want))
			}
		})
	}
}

func TestR3RealExhaustionWinsOverContendingLease(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_MACHINE_DAILY_CALLS", "2")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", strconv.FormatFloat(capacityTestDuration(4*time.Second).Seconds(), 'f', -1, 64))
	ctx, cancel := context.WithTimeout(t.Context(), capacityTestDuration(5*time.Second))
	defer cancel()
	ctx = ContextWithConfig(ctx, Config{LedgerPath: path, TaskID: "holder", MaxCalls: 10})
	first, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	done := make(chan error, 1)
	go func() {
		waiter, err := Reserve(ContextWithConfig(ctx, Config{TaskID: "waiter"}), Metadata{Engine: "codex"})
		if waiter != nil {
			_ = waiter.Complete(Outcome{Status: Passed, Known: true})
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("busy admission did not wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	other, err := Reserve(ContextWithConfig(ctx, Config{TaskID: "other"}), Metadata{Engine: "grok"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := other.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("real exhaustion was hidden by contention: %v", err)
		}
	case <-time.After(capacityTestDuration(2 * time.Second)):
		t.Fatal("real exhaustion waited for the lease cap")
	}
	if len(readTestLedger(t, path)["attempts"].([]any)) != 2 {
		t.Fatal("busy/exhausted admission was charged")
	}
}

func TestR3BusyDeadlineAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancellation"}[canceled], func(t *testing.T) {
			path := layeredFixture(t)
			t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
			t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "")
			ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "holder", MaxCalls: 10})
			first, err := Reserve(ctx, Metadata{Engine: "agy"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := first.Complete(Outcome{Status: Passed, Known: true}); err != nil {
					t.Fatal(err)
				}
			}()
			waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			want := ErrConcurrencyBusy
			if canceled {
				timer := time.AfterFunc(25*time.Millisecond, cancel)
				defer timer.Stop()
				want = context.Canceled
			}
			if _, err := Reserve(waitCtx, Metadata{Engine: "agy"}); !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			if len(readTestLedger(t, path)["attempts"].([]any)) != 1 {
				t.Fatal("timed out admission was charged")
			}
		})
	}
}
