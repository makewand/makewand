package router

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestQuotaProviderFilterPreventsReadsAndFiltersCachedEntries(t *testing.T) {
	disabled := &countingSource{name: "gemini", q: ProviderQuota{HasData: true, Authed: true}}
	enabled := &countingSource{name: "codex", q: ProviderQuota{HasData: true, Authed: true}}
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	seed := NewQuotaSnapshotter(time.Hour, disabled, enabled).WithDiskCache(cache, time.Hour)
	seed.Refresh(context.Background())
	disabledCalls := disabled.reads.Load()
	enabledCalls := enabled.reads.Load()
	filtered := NewQuotaSnapshotter(time.Hour, disabled, enabled).WithProviderFilter(func(name string) bool { return name != "gemini" }).WithDiskCache(cache, time.Hour)
	filtered.LoadCache()
	if _, ok := filtered.Snapshot().Get("gemini"); ok {
		t.Fatal("disabled provider restored from cached snapshot")
	}
	filtered.Refresh(context.Background())
	if _, ok := filtered.Snapshot().Get("gemini"); ok {
		t.Fatal("disabled provider restored on cache refresh")
	}
	if _, ok := filtered.Snapshot().Get("codex"); !ok {
		t.Fatal("enabled cached provider missing")
	}
	if disabled.reads.Load() != disabledCalls || enabled.reads.Load() != enabledCalls {
		t.Fatal("fresh cache caused source reads")
	}
	filtered.WithDiskCache("", 0).Refresh(context.Background())
	if disabled.reads.Load() != disabledCalls || enabled.reads.Load() != enabledCalls+1 {
		t.Fatal("filter did not prevent source reads")
	}
}
