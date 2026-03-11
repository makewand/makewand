package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchOverrides_ReloadsOnChange(t *testing.T) {
	dir := t.TempDir()

	r := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude": {Provider: &stubProvider{name: "claude", available: true}, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})

	// Record trace events
	var traces []TraceEvent
	r.SetTraceSink(TraceSinkFunc(func(e TraceEvent) {
		traces = append(traces, e)
	}))

	// Start watcher with fast polling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.WatchOverridesInterval(ctx, dir, 50*time.Millisecond)

	// Initially no routing.json — nothing happens
	time.Sleep(100 * time.Millisecond)

	// Write a valid routing.json override (must satisfy validation)
	override := `{
		"models": {
			"claude": {"cheap": "claude-3-haiku-20240307", "mid": "claude-sonnet-4-20250514", "premium": "claude-opus-4-20250514"},
			"codex": {"cheap": "gpt-4.1-mini", "mid": "gpt-4.1-mini", "premium": "gpt-4.1"},
			"gemini": {"cheap": "gemini-2.0-flash", "mid": "gemini-2.0-flash", "premium": "gemini-2.5-pro"}
		},
		"costs": {
			"claude-3-haiku-20240307": {"Input": 0.25, "Output": 1.25},
			"claude-sonnet-4-20250514": {"Input": 3.0, "Output": 15.0},
			"claude-opus-4-20250514": {"Input": 15.0, "Output": 75.0},
			"gpt-4.1-mini": {"Input": 0.4, "Output": 1.6},
			"gpt-4.1": {"Input": 2.0, "Output": 8.0},
			"gemini-2.0-flash": {"Input": 0.1, "Output": 0.4},
			"gemini-2.5-pro": {"Input": 1.25, "Output": 10.0}
		}
	}`
	path := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(path, []byte(override), 0600); err != nil {
		t.Fatal(err)
	}

	// Wait for reload
	time.Sleep(200 * time.Millisecond)

	// Verify trace event was emitted
	found := false
	for _, e := range traces {
		if e.Event == "reload_success" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected reload_success trace event")
	}
}

func TestWatchOverrides_StopsOnCancel(t *testing.T) {
	dir := t.TempDir()

	r := NewRouterFromConfig(RouterConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	r.WatchOverridesInterval(ctx, dir, 50*time.Millisecond)

	// Cancel and verify it doesn't panic
	cancel()
	time.Sleep(100 * time.Millisecond)
}

func TestWatchOverrides_IgnoresBadJSON(t *testing.T) {
	dir := t.TempDir()

	r := NewRouterFromConfig(RouterConfig{})

	var traces []TraceEvent
	r.SetTraceSink(TraceSinkFunc(func(e TraceEvent) {
		traces = append(traces, e)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.WatchOverridesInterval(ctx, dir, 50*time.Millisecond)

	// Write invalid JSON
	path := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(path, []byte("{invalid json}"), 0600); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Should have a reload_error trace
	found := false
	for _, e := range traces {
		if e.Event == "reload_error" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected reload_error trace event for bad JSON")
	}
}
