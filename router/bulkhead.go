package router

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const defaultAPIConcurrency = 8
const defaultCLIConcurrency = 1
const defaultProviderQueueTimeout = 30 * time.Second

type providerBulkheads struct {
	mu       sync.Mutex
	permits  map[string]chan struct{}
	api, cli int
	wait     time.Duration
}

func newProviderBulkheads(api, cli int, wait time.Duration) (*providerBulkheads, error) {
	if api == 0 {
		api = defaultAPIConcurrency
	}
	if cli == 0 {
		cli = defaultCLIConcurrency
	}
	if wait == 0 {
		wait = defaultProviderQueueTimeout
	}
	if api < 1 || api > 1024 || cli < 1 || cli > 1024 || wait < 0 {
		return nil, fmt.Errorf("provider concurrency must be 1..1024 and queue timeout positive")
	}
	return &providerBulkheads{permits: make(map[string]chan struct{}), api: api, cli: cli, wait: wait}, nil
}

func (r *Router) bulkheadRoot() *Router {
	if r.cacheRoot != nil {
		return r.cacheRoot
	}
	return r
}
func (r *Router) providerBulkheads() *providerBulkheads {
	root := r.bulkheadRoot()
	root.providerMu.Lock()
	defer root.providerMu.Unlock()
	if root.bulkheads == nil {
		root.bulkheads, _ = newProviderBulkheads(0, 0, 0)
	}
	return root.bulkheads
}

// ConfigureConcurrency overrides constructor defaults before the first provider
// admission. Request-scoped views share the root's permits, and an active router
// cannot replace its semaphore to evade a configured bound.
func (r *Router) ConfigureConcurrency(api, cli int, wait time.Duration) error {
	replacement, err := newProviderBulkheads(api, cli, wait)
	if err != nil {
		return err
	}
	b := r.providerBulkheads()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.permits) != 0 {
		return fmt.Errorf("provider concurrency cannot change after first admission")
	}
	b.api, b.cli, b.wait = replacement.api, replacement.cli, replacement.wait
	return nil
}

func (r *Router) acquireProvider(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := r.providerBulkheads()
	access := r.bulkheadRoot().getAccessType(name)
	b.mu.Lock()
	permit := b.permits[name]
	if permit == nil {
		limit := b.api
		if access == AccessSubscription || access == AccessLocal {
			limit = b.cli
		}
		permit = make(chan struct{}, limit)
		b.permits[name] = permit
	}
	wait := b.wait
	b.mu.Unlock()
	queueCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	select {
	case permit <- struct{}{}:
		if err := queueCtx.Err(); err != nil {
			<-permit
			return nil, err
		}
	case <-queueCtx.Done():
		return nil, queueCtx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-permit }) }, nil
}
