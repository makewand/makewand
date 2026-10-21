//go:build !windows

package router

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO among the session logs is skipped, not opened: opening it would
// block the refresh until some process writes to it.
func TestCodexQuotaSkipsANonRegularSessionLog_GR5(t *testing.T) {
	root := t.TempDir()
	writeQuotaTestAuth(t, root, "A")
	sessions := filepath.Join(root, "sessions")
	writeQuotaTestLog(t, root, quotaTestEvent("A", 30, 300, time.Now().Add(-time.Minute), ""))
	if err := syscall.Mkfifo(filepath.Join(sessions, "fifo.jsonl"), 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan ProviderQuota, 1)
	go func() {
		q, _ := NewCodexQuotaSource(sessions).Read(context.Background())
		done <- q
	}()
	select {
	case q := <-done:
		if q.FiveHourPct == nil || *q.FiveHourPct != 30 {
			t.Fatalf("five-hour window = %s, want 30 from the regular log", pctString(q.FiveHourPct))
		}
	case <-time.After(10 * time.Second):
		// Unblock the reader stuck in open so the test can end.
		if w, err := os.OpenFile(filepath.Join(sessions, "fifo.jsonl"), os.O_WRONLY, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("reading the session logs blocked on a FIFO")
	}
}
