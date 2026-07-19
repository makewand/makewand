// registry.go — provider registry methods extracted from router.go.
// These methods manage provider lookup, registration, caching and availability.

package router

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (r *Router) registeredProviderNames() []string {
	r.providerMu.Lock()
	defer r.providerMu.Unlock()

	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		if !r.isProviderAllowed(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveProvider returns a provider instance for the given (name, modelID) pair.
// It caches instances so the same (provider, model) combination reuses the same instance.
func (r *Router) resolveProvider(providerName, modelID string) (Provider, error) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if !r.isProviderAllowed(providerName) {
		return nil, fmt.Errorf("provider %q is not allowed", providerName)
	}
	key := providerKey{name: providerName, modelID: modelID}

	r.providerMu.Lock()

	if p, ok := r.providerCache[key]; ok {
		r.providerMu.Unlock()
		return p, nil
	}

	// Reuse explicitly registered providers when they are model-agnostic, or
	// when no factory exists to materialize model-specific instances.
	if existing, ok := r.providers[providerName]; ok {
		if _, isCLI := existing.(*CLIProvider); isCLI || modelID == "" {
			r.providerCache[key] = existing
			r.providerMu.Unlock()
			return existing, nil
		}
		if _, hasFactory := r.getFactoryLocked(providerName); !hasFactory {
			r.providerCache[key] = existing
			r.providerMu.Unlock()
			return existing, nil
		}
	}

	factory, ok := r.getFactoryLocked(providerName)
	r.providerMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}

	// Construct outside the lock: factories may probe binaries or the network,
	// and holding providerMu here would stall every concurrent route.
	p, err := factory(modelID)
	if err != nil {
		return nil, err
	}
	// Price from this Router's snapshot. Safe before publication: p is still
	// exclusively owned by this goroutine until it is cached below.
	r.attachCostTable(p)

	// Re-acquire and double-check: a concurrent caller may have cached an
	// instance for the same key while the factory ran. Keep the first one so
	// every caller shares the same instance.
	r.providerMu.Lock()
	defer r.providerMu.Unlock()
	if existing, ok := r.providerCache[key]; ok {
		return existing, nil
	}
	r.providerCache[key] = p
	return p, nil
}

// Get returns a specific provider by name.
func (r *Router) Get(name string) (Provider, error) {
	if !r.isProviderAllowed(name) {
		return nil, newProviderError(name, "lookup", ErrorKindConfig, false, 0, fmt.Sprintf("model provider %q is not allowed", name), nil)
	}
	r.providerMu.Lock()
	p, ok := r.providers[name]
	r.providerMu.Unlock()
	if !ok {
		return nil, newProviderError(name, "lookup", ErrorKindConfig, false, 0, fmt.Sprintf("model provider %q not configured", name), nil)
	}
	// Untrusted-repo mode: never hand back — nor even probe — a provider that runs
	// a repo-aware host agent. Get returns the Provider to the caller, who could
	// then invoke .Chat directly and bypass the router's gates. Fail closed by
	// returning the same not-found contract as an unconfigured provider, and
	// crucially before IsAvailable so no host health check is exec'd against the
	// untrusted repo. untrustedRepoSafe is a no-op in trusted mode, so trusted
	// behavior (including the probe below) is byte-for-byte unchanged.
	if !r.untrustedRepoSafe(p) {
		return nil, newProviderError(name, "lookup", ErrorKindConfig, false, 0, fmt.Sprintf("model provider %q not configured", name), nil)
	}
	if !p.IsAvailable() {
		return nil, newProviderError(name, "lookup", ErrorKindConfig, false, 0, fmt.Sprintf("model provider %q is not available", name), nil)
	}
	return p, nil
}

// RegisterProvider injects a provider instance at runtime.
// This is useful for private/custom providers without editing router internals.
func (r *Router) RegisterProvider(name string, provider Provider, access AccessType) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("provider name is empty")
	}
	if provider == nil {
		return fmt.Errorf("provider is nil")
	}

	r.providerMu.Lock()
	r.providers[name] = provider
	// Collect stale cache keys before deleting to avoid map mutation during iteration.
	var staleKeys []providerKey
	for k := range r.providerCache {
		if k.name == name {
			staleKeys = append(staleKeys, k)
		}
	}
	for _, k := range staleKeys {
		delete(r.providerCache, k)
	}
	r.providerCache[providerKey{name: name, modelID: ""}] = provider
	r.accessTypes[name] = access
	r.attachCostTable(provider)
	r.providerMu.Unlock()

	r.mu.Lock()
	r.cachedAvailAt = time.Time{}
	r.mu.Unlock()
	return nil
}

// Available returns all available provider names, filtered by the effective mode.
// Results are filtered by circuit breaker state.
// Results are cached to avoid repeated health checks on every render cycle.
func (r *Router) Available() []string {
	r.mu.Lock()
	if time.Since(r.cachedAvailAt) < availCacheTTL {
		names := r.cachedAvail
		r.mu.Unlock()
		return names
	}
	r.mu.Unlock()

	// Snapshot the provider set under the lock, then probe outside it: a cold
	// CLI health check execs a subprocess with a multi-second timeout, and
	// holding r.mu/r.providerMu for that long would stall every concurrent route.
	type providerProbe struct {
		name string
		p    Provider
	}
	r.providerMu.Lock()
	probes := make([]providerProbe, 0, len(r.providers))
	for name, p := range r.providers {
		if !r.isProviderAllowed(name) {
			continue
		}
		probes = append(probes, providerProbe{name: name, p: p})
	}
	r.providerMu.Unlock()

	var names []string
	for _, probe := range probes {
		// Exclude providers that are not untrusted-repo-safe before probing, so an
		// unsafe CLI is never exec'd for a health check in untrusted mode.
		// untrustedRepoSafe is a no-op in trusted mode, so behavior is unchanged there.
		if !r.untrustedRepoSafe(probe.p) {
			continue
		}
		if probe.p.IsAvailable() {
			if blocked, _ := r.isCircuitOpen(probe.name); blocked {
				continue
			}
			names = append(names, probe.name)
		}
	}
	sort.Strings(names)

	r.mu.Lock()
	r.cachedAvail = names
	r.cachedAvailAt = time.Now()
	r.mu.Unlock()
	return names
}

// getProvider returns a provider by name (thread-safe).
func (r *Router) getProvider(name string) (Provider, bool) {
	if !r.isProviderAllowed(name) {
		return nil, false
	}
	r.providerMu.Lock()
	defer r.providerMu.Unlock()
	p, ok := r.providers[name]
	return p, ok
}

func (r *Router) remoteOnlyProvider() (string, Provider, bool) {
	if !r.isProviderAllowed("remote") {
		return "", nil, false
	}
	// Snapshot under the lock, then probe outside it: IsAvailable may exec a CLI
	// or hit the network, and holding providerMu across that would stall every
	// concurrent route (round-2/3 lock discipline).
	r.providerMu.Lock()
	only := len(r.providers) == 1
	p, ok := r.providers["remote"]
	r.providerMu.Unlock()

	if !only || !ok || p == nil {
		return "", nil, false
	}
	// Fail closed in untrusted mode: a provider registered under the name "remote"
	// that is not untrusted-repo-safe (e.g. a custom/non-capable CLI) must not
	// bypass the capability gate via this fast path. untrustedRepoSafe is a pure
	// type assertion and a no-op in trusted mode, so trusted behavior is unchanged.
	if !r.untrustedRepoSafe(p) {
		return "", nil, false
	}
	if !p.IsAvailable() {
		return "", nil, false
	}
	return "remote", p, true
}

// IsSubscription returns true if the named provider uses subscription access.
func (r *Router) IsSubscription(name string) bool {
	r.providerMu.Lock()
	defer r.providerMu.Unlock()
	return r.accessTypes[name] == AccessSubscription
}

func (r *Router) isBuildProviderAvailable(name, modelID string) bool {
	if !r.isProviderAllowed(name) {
		return false
	}
	r.providerMu.Lock()
	p, ok := r.providerCache[providerKey{name: name, modelID: modelID}]
	if !ok {
		p, ok = r.providers[name]
	}
	r.providerMu.Unlock()
	if !ok {
		return false
	}
	// Exclude providers that are not untrusted-repo-safe when untrusted mode is
	// active. Checked before IsAvailable so an unsafe CLI is never even probed.
	if !r.untrustedRepoSafe(p) {
		return false
	}
	// Probe outside the lock: a cold CLI health check execs a subprocess.
	return p.IsAvailable()
}

// tryBuildProvider resolves a provider instance for the given name and tier.
func (r *Router) tryBuildProvider(name string, tier ModelTier) (Provider, string, error) {
	models, ok := r.routingTables().modelsFor(name)
	if !ok {
		// Dynamically registered providers may not have a modelTable entry.
		p, err := r.resolveProvider(name, "")
		if err != nil {
			return nil, "", fmt.Errorf("no model table entry for %s and dynamic resolution failed: %w", name, err)
		}
		return p, "", nil
	}
	modelID := models[tier]
	p, err := r.resolveProvider(name, modelID)
	if err != nil {
		return nil, "", err
	}
	return p, modelID, nil
}
