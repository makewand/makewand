// reload.go — Strategy table hot-reload via file system polling.
//
// WatchOverrides starts a background goroutine that periodically checks
// routing.json for changes and merges them into this Router's strategy tables.
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
// for modifications. When the file changes, the new data is validated and
// deep-merged into this Router's strategy tables; other Router instances are
// unaffected. Invalid overrides keep the previous tables. The goroutine stops
// when ctx is canceled.
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
					if os.IsNotExist(err) {
						// The override file was removed. Revert to the immutable
						// defaults exactly once, then stay quiet until a new file
						// appears. A zero lastMod means no override was ever active.
						prev := lastMod.Load().(time.Time)
						if prev.IsZero() {
							continue
						}
						r.routingTables().resetToDefaults()
						lastMod.Store(time.Time{})
						r.emitTrace(TraceEvent{
							Event:  "reload_success",
							Detail: "routing.json removed; reverted to defaults",
						})
						continue
					}
					// Other stat errors (permissions, transient FS issues): surface
					// and skip without touching the live tables.
					r.emitTrace(TraceEvent{
						Event: "reload_error",
						Error: "stat routing.json: " + err.Error(),
					})
					continue
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

				// applyOverrides validates the merged candidate before swapping
				// it in; on failure the previous tables stay active.
				if err := r.routingTables().applyOverrides(data); err != nil {
					r.emitTrace(TraceEvent{
						Event: "reload_error",
						Error: "applying routing.json: " + err.Error(),
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
