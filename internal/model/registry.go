// registry.go — provider registry methods extracted from router.go.
// These methods manage provider lookup, registration, caching and availability.

package model

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
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveProvider returns a provider instance for the given (name, modelID) pair.
// It caches instances so the same (provider, model) combination reuses the same instance.
func (r *Router) resolveProvider(providerName, modelID string) (Provider, error) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	key := providerKey{name: providerName, modelID: modelID}

	r.providerMu.Lock()
	defer r.providerMu.Unlock()

	if p, ok := r.providerCache[key]; ok {
		return p, nil
	}

	// If a CLI provider is already registered for this provider name, use it
	// (CLI providers ignore modelID — they use subscription defaults)
	if existing, ok := r.providers[providerName]; ok {
		if _, isCLI := existing.(*CLIProvider); isCLI {
			r.providerCache[key] = existing
			return existing, nil
		}
	}

	resolver, ok := getProviderResolver(providerName)
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}
	p, err := resolver(r.cfg, modelID)
	if err != nil {
		return nil, err
	}

	r.providerCache[key] = p
	return p, nil
}

// Get returns a specific provider by name.
func (r *Router) Get(name string) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
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
	for k := range r.providerCache {
		if k.name == name {
			delete(r.providerCache, k)
		}
	}
	r.providerCache[providerKey{name: name, modelID: ""}] = provider
	r.providerMu.Unlock()

	r.mu.Lock()
	r.accessTypes[name] = access
	r.cachedAvailAt = time.Time{}
	r.mu.Unlock()
	return nil
}

// Available returns all available provider names, filtered by the effective mode.
// Results are filtered by circuit breaker state.
// Results are cached to avoid repeated health checks on every render cycle.
func (r *Router) Available() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.cachedAvailAt) < availCacheTTL {
		return r.cachedAvail
	}

	var names []string
	for name, p := range r.providers {
		if p.IsAvailable() {
			if blocked, _ := r.isCircuitOpen(name); blocked {
				continue
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)

	r.cachedAvail = names
	r.cachedAvailAt = time.Now()
	return names
}

// IsSubscription returns true if the named provider uses subscription access.
func (r *Router) IsSubscription(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accessTypes[name] == AccessSubscription
}

func (r *Router) isBuildProviderAvailable(name, modelID string) bool {
	r.providerMu.Lock()
	defer r.providerMu.Unlock()

	if p, ok := r.providerCache[providerKey{name: name, modelID: modelID}]; ok {
		return p.IsAvailable()
	}
	if p, ok := r.providers[name]; ok {
		return p.IsAvailable()
	}
	return false
}

// tryBuildProvider resolves a provider instance for the given name and tier.
func (r *Router) tryBuildProvider(name string, tier ModelTier) (Provider, string, error) {
	models, ok := modelTable[name]
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
