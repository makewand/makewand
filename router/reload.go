// reload.go — Strategy table hot-reload via file system polling.
//
// WatchOverrides starts a background goroutine that periodically checks
// routing.json for changes and merges them into the package-level strategy tables.
// No external dependencies (fsnotify) required — uses simple stat-based polling.
package router

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// DefaultReloadInterval is the default polling interval for routing.json changes.
const DefaultReloadInterval = 30 * time.Second

// WatchOverrides starts a background goroutine that polls configDir/routing.json
// for modifications. When the file changes, the new data is merged into the
// package-level strategy tables. The goroutine stops when ctx is canceled.
//
// Returns immediately. Call with a cancellable context to stop the watcher.
// Errors during reload are traced (if a TraceSink is set) but do not stop the watcher.
func (r *Router) WatchOverrides(ctx context.Context, configDir string) {
	r.WatchOverridesInterval(ctx, configDir, DefaultReloadInterval)
}

// WatchOverridesInterval is like WatchOverrides but with a custom polling interval.
func (r *Router) WatchOverridesInterval(ctx context.Context, configDir string, interval time.Duration) {
	if configDir == "" || interval <= 0 {
		return
	}

	path := filepath.Join(configDir, "routing.json")
	var lastMod atomic.Value
	lastMod.Store(time.Time{})

	// Read initial mod time
	if info, err := os.Stat(path); err == nil {
		lastMod.Store(info.ModTime())
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				info, err := os.Stat(path)
				if err != nil {
					continue // file doesn't exist or is inaccessible
				}

				prev := lastMod.Load().(time.Time)
				if !info.ModTime().After(prev) {
					continue // unchanged
				}

				data, err := os.ReadFile(path)
				if err != nil {
					r.emitTrace(TraceEvent{
						Event: "reload_error",
						Error: "reading routing.json: " + err.Error(),
					})
					continue
				}

				if err := loadDefaults(data); err != nil {
					r.emitTrace(TraceEvent{
						Event: "reload_error",
						Error: "parsing routing.json: " + err.Error(),
					})
					continue
				}

				lastMod.Store(info.ModTime())
				r.emitTrace(TraceEvent{
					Event:  "reload_success",
					Detail: "routing.json reloaded at " + info.ModTime().Format(time.RFC3339),
				})
			}
		}
	}()
}
