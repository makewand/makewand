package router

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ProviderFactory constructs a provider instance for the given model ID.
// This is the config-free version used by the library API.
type ProviderFactory func(modelID string) (Provider, error)

var (
	resolverMu       sync.RWMutex
	providerFactory  = map[string]ProviderFactory{}
)

// RegisterProviderFactory registers or overrides a provider factory.
func RegisterProviderFactory(name string, factory ProviderFactory) error {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return fmt.Errorf("provider name is empty")
	}
	if factory == nil {
		return fmt.Errorf("factory is nil")
	}

	resolverMu.Lock()
	providerFactory[name] = factory
	resolverMu.Unlock()
	return nil
}

func getProviderFactory(name string) (ProviderFactory, bool) {
	resolverMu.RLock()
	defer resolverMu.RUnlock()
	r, ok := providerFactory[name]
	return r, ok
}

func factoryNames() []string {
	resolverMu.RLock()
	defer resolverMu.RUnlock()
	names := make([]string, 0, len(providerFactory))
	for name := range providerFactory {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
