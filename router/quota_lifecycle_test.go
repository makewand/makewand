package router

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingSource records how many times Read is invoked so tests can assert that
// LoadCache never touches a source and that Start refreshes exactly once.
type countingSource struct {
	name  string
	q     ProviderQuota
	reads atomic.Int64
}

func (c *countingSource) Provider() string { return c.name }

func (c *countingSource) Read(context.Context) (ProviderQuota, error) {
	c.reads.Add(1)
	return c.q, nil
}

func TestStartIsIdempotent(t *testing.T) {
	src := &countingSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(40)}}
	// A long interval keeps the ticker from firing during the test, so the read
	// count reflects only the synchronous first-refresh(es).
	s := NewQuotaSnapshotter(time.Hour, src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx) // one synchronous refresh + one ticker goroutine
	s.Start(ctx) // must be a full no-op: no second refresh, no second ticker
	s.Start(ctx)

	if got := src.reads.Load(); got != 1 {
		t.Fatalf("repeated Start should refresh exactly once, got %d reads", got)
	}
	if !s.started.Load() {
		t.Fatal("started flag should be set after Start")
	}
}

func TestLoadCacheIsNetworkFree(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "q.json")

	// Seed the shared cache via a normal refresh from a throwaway snapshotter.
	seed := &fakeSource{name: "claude", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(55)}}
	NewQuotaSnapshotter(time.Hour, seed).WithDiskCache(cache, time.Hour).Refresh(context.Background())

	// A fresh snapshotter whose source must never be read on the warm-start path.
	src := &countingSource{name: "claude", q: ProviderQuota{HasData: true, WeeklyPct: fptr(1)}}
	s := NewQuotaSnapshotter(time.Hour, src).WithDiskCache(cache, time.Hour)
	s.LoadCache()

	if got := src.reads.Load(); got != 0 {
		t.Fatalf("LoadCache must not read any source, got %d reads", got)
	}
	q, ok := s.Snapshot().Get("claude")
	if !ok || q.WeeklyPct == nil || *q.WeeklyPct != 55 {
		t.Fatalf("LoadCache should populate last-good 55 from disk, got %+v ok=%v", q, ok)
	}
}

func TestLoadCacheAdoptsStaleLastGood(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "q.json")

	// Write the cache, then let it age past a 1ns TTL so it is "stale".
	seed := &fakeSource{name: "codex", q: ProviderQuota{HasData: true, Authed: true, WeeklyPct: fptr(33)}}
	NewQuotaSnapshotter(time.Hour, seed).WithDiskCache(cache, time.Nanosecond).Refresh(context.Background())
	time.Sleep(2 * time.Millisecond)

	src := &countingSource{name: "codex"}
	s := NewQuotaSnapshotter(time.Hour, src).WithDiskCache(cache, time.Nanosecond)
	s.LoadCache() // adopts last-good regardless of TTL, without reading the source

	if got := src.reads.Load(); got != 0 {
		t.Fatalf("LoadCache must not read any source, got %d reads", got)
	}
	q, ok := s.Snapshot().Get("codex")
	if !ok || q.WeeklyPct == nil || *q.WeeklyPct != 33 {
		t.Fatalf("LoadCache should adopt stale last-good 33, got %+v ok=%v", q, ok)
	}
}

func TestLoadCacheNoCacheIsNoop(t *testing.T) {
	src := &countingSource{name: "claude", q: ProviderQuota{HasData: true, WeeklyPct: fptr(9)}}
	s := NewQuotaSnapshotter(time.Hour, src) // no disk cache configured
	s.LoadCache()

	if got := src.reads.Load(); got != 0 {
		t.Fatalf("LoadCache must never read a source, got %d", got)
	}
	if _, ok := s.Snapshot().Get("claude"); ok {
		t.Fatal("LoadCache with no cache should leave the snapshot empty")
	}
}
