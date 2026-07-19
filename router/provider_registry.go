package router

import (
	"fmt"
	"strings"
)

// ProviderFactory constructs a provider instance for the given model ID.
// This is the config-free version used by the library API.
type ProviderFactory func(modelID string) (Provider, error)

// RegisterProviderFactory registers or overrides a provider factory on this
// Router. Factories are strictly per-instance, so two Routers can build the
// same provider name from different configurations without ever affecting one
// another. There is no package-level factory registry.
func (r *Router) RegisterProviderFactory(name string, factory ProviderFactory) error {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return fmt.Errorf("provider name is empty")
	}
	if factory == nil {
		return fmt.Errorf("factory is nil")
	}

	r.providerMu.Lock()
	if r.factories == nil {
		r.factories = make(map[string]ProviderFactory)
	}
	r.factories[name] = factory
	r.providerMu.Unlock()
	return nil
}

// getFactoryLocked returns the factory registered for a provider name on this
// Router. It assumes r.providerMu is held (called from resolveProvider).
// Resolution is fully per-instance: a registration on one Router can never
// leak into another.
func (r *Router) getFactoryLocked(name string) (ProviderFactory, bool) {
	factory, ok := r.factories[name]
	return factory, ok
}

// costTableAware is implemented by providers that price completions from a
// strategyTables snapshot (the API providers Claude/Gemini/OpenAI).
type costTableAware interface {
	useCostTable(t *strategyTables)
}

// attachCostTable stores this Router's price snapshot on a provider as a fallback
// for routerless pricing. The per-call price snapshot the Router injects into the
// request context (see contextWithCostTable) takes precedence during Chat, so a
// provider instance shared by two Routers is priced per-Router and never mispriced
// by a later Router's attachCostTable overwriting an earlier snapshot.
// Providers without the capability are left as-is.
func (r *Router) attachCostTable(p Provider) {
	if aware, ok := p.(costTableAware); ok {
		aware.useCostTable(r.routingTables())
	}
}
