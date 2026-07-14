package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskCacheReadThrough(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "quota-snapshot.json")

	src := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(40)}}
	s := NewQuotaSnapshotter(time.Hour, src).WithDiskCache(cache, time.Hour)
	s.Refresh(context.Background())

	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache file should be written: %v", err)
	}

	// Change the source, but a fresh (within-TTL) cache must be served instead.
	src.set(ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(99)}, nil)
	s.Refresh(context.Background())
	q, _ := s.Snapshot().Get("claude")
	if q.WeeklyPct == nil || *q.WeeklyPct != 40 {
		t.Fatalf("within TTL should serve cached 40, got %v", q.WeeklyPct)
	}

	// A second snapshotter (fresh process) reads the same cache without hitting
	// any source.
	deadSrc := &fakeSource{name: "claude", err: context.DeadlineExceeded}
	s2 := NewQuotaSnapshotter(time.Hour, deadSrc).WithDiskCache(cache, time.Hour)
	s2.Refresh(context.Background())
	q2, ok := s2.Snapshot().Get("claude")
	if !ok || q2.WeeklyPct == nil || *q2.WeeklyPct != 40 {
		t.Fatalf("second process should read cached 40, got %+v ok=%v", q2, ok)
	}
}

func TestDiskCacheTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "q.json")

	src := &fakeSource{name: "codex", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(50)}}
	// TTL of 1ns → the cache is always considered stale, so sources are re-read.
	s := NewQuotaSnapshotter(time.Hour, src).WithDiskCache(cache, time.Nanosecond)
	s.Refresh(context.Background())

	src.set(ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(77)}, nil)
	time.Sleep(2 * time.Millisecond)
	s.Refresh(context.Background())
	q, _ := s.Snapshot().Get("codex")
	if q.WeeklyPct == nil || *q.WeeklyPct != 77 {
		t.Fatalf("expired cache should re-read source (77), got %v", q.WeeklyPct)
	}
}

func TestStaleCacheServesAsLastGoodOnFailure(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "q.json")

	// First: a good read populates the cache.
	good := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(64)}}
	NewQuotaSnapshotter(time.Hour, good).WithDiskCache(cache, time.Nanosecond).Refresh(context.Background())
	time.Sleep(2 * time.Millisecond) // cache now stale (TTL 1ns)

	// A fresh process whose source now fails should fall back to the cached 64%,
	// not show "no data".
	failing := &fakeSource{name: "claude", err: context.DeadlineExceeded}
	s := NewQuotaSnapshotter(time.Hour, failing).WithDiskCache(cache, time.Nanosecond)
	s.Refresh(context.Background())
	q, ok := s.Snapshot().Get("claude")
	if !ok || !q.HasData || q.WeeklyPct == nil || *q.WeeklyPct != 64 {
		t.Fatalf("failed read should retain cached last-good 64, got %+v ok=%v", q, ok)
	}
}

func TestDiskCacheDoesNotPersistSeals(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "q.json")

	src := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(30)}}
	s := NewQuotaSnapshotter(time.Hour, src).WithDiskCache(cache, time.Hour)
	s.Refresh(context.Background())
	s.MarkExhausted("claude", time.Now().Add(time.Hour))

	// The seal shows 100% in this process...
	q, _ := s.Snapshot().Get("claude")
	if used, _ := q.EffectiveUsedPct(); used != 100 {
		t.Fatalf("sealed provider should read 100 in-process, got %.0f", used)
	}

	// ...but a fresh process reading the cache must see the raw 30%, not a seal.
	s2 := NewQuotaSnapshotter(time.Hour, src).WithDiskCache(cache, time.Hour)
	s2.Refresh(context.Background())
	q2, _ := s2.Snapshot().Get("claude")
	if used, _ := q2.EffectiveUsedPct(); used != 30 {
		t.Fatalf("cache must not persist the seal; fresh process should read 30, got %.0f", used)
	}
}
