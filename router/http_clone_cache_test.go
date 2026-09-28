package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/makewand/makewand/serverauth"
)

func newCountingFactoryRouter(t *testing.T, calls *atomic.Int64) *Router {
	t.Helper()
	r, err := NewRouterFromConfig(RouterConfig{
		Providers: map[string]ProviderEntry{
			// A registered non-CLI provider plus a factory: model-specific
			// instances are materialized through the factory (the serve setup).
			"claude": {Provider: &stubProvider{name: "claude", available: true}, Access: AccessSubscription},
		},
		UsageMode: "balanced",
	})
	if err != nil {
		t.Fatalf("NewRouterFromConfig: %v", err)
	}
	if err := r.RegisterProviderFactory("claude", func(modelID string) (Provider, error) {
		calls.Add(1)
		return &stubProvider{name: "claude", available: true, response: "factory:" + modelID}, nil
	}); err != nil {
		t.Fatalf("RegisterProviderFactory: %v", err)
	}
	return r
}

func postChat(t *testing.T, h http.Handler, token, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestHTTPPerRequestClonesReuseFactoryProviders reproduces go-router#4: every
// request carrying `mode` (or a token provider allowlist) got a cloned Router
// whose factory-built providers lived only in the clone's cache, so the factory
// — and for API providers a fresh pair of http.Transports — ran on every request.
func TestHTTPPerRequestClonesReuseFactoryProviders(t *testing.T) {
	authz, err := serverauth.NewAuthorizer(serverauth.Config{
		Tokens: []serverauth.TokenRule{{
			Token:            "allowlisted",
			Scopes:           []string{serverauth.ScopeChatInvoke},
			AllowedProviders: []string{"claude"},
		}},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	cases := []struct {
		name  string
		opts  []HTTPHandlerOptions
		token string
		body  string
	}{
		{name: "mode clone", body: `{"mode":"fast","messages":[{"role":"user","content":"write a hello world"}]}`},
		{name: "allowlist clone", opts: []HTTPHandlerOptions{{Authorizer: authz}}, token: "allowlisted",
			body: `{"messages":[{"role":"user","content":"write a hello world"}]}`},
		{name: "mode + allowlist clone of clone", opts: []HTTPHandlerOptions{{Authorizer: authz}}, token: "allowlisted",
			body: `{"mode":"fast","messages":[{"role":"user","content":"write a hello world"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			r := newCountingFactoryRouter(t, &calls)
			h := r.HTTPHandler(tc.opts...)
			const requests = 5
			for i := 0; i < requests; i++ {
				postChat(t, h, tc.token, tc.body)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("factory ran %d times for %d identical requests; want 1 (instances must be cached on the long-lived Router)", got, requests)
			}
		})
	}
}

// TestPerRequestClonesShareFactoryInstancesConcurrently: concurrent clones must
// converge on one instance per (provider, model) and stay race-free.
func TestPerRequestClonesShareFactoryInstancesConcurrently(t *testing.T) {
	var calls atomic.Int64
	r := newCountingFactoryRouter(t, &calls)
	var wg sync.WaitGroup
	got := make([]Provider, 16)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			clone := r.cloneWithMode(ModeFast).cloneWithProviderAllowlist([]string{"claude"})
			p, err := clone.resolveProvider("claude", "model-x")
			if err != nil {
				t.Errorf("resolveProvider: %v", err)
				return
			}
			got[i] = p
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatalf("clone %d resolved a different instance; clones must share the root's cached provider", i)
		}
	}
	if p, err := r.resolveProvider("claude", "model-x"); err != nil || p != got[0] {
		t.Fatalf("root resolveProvider = %v, %v; want the instance the clones cached", p, err)
	}
}

// TestReRegisteredFactoryInvalidatesCachedInstances: once instances are shared
// through the root cache, re-registering a factory must drop the stale ones.
func TestReRegisteredFactoryInvalidatesCachedInstances(t *testing.T) {
	var first atomic.Int64
	r := newCountingFactoryRouter(t, &first)
	if _, err := r.cloneWithMode(ModeFast).resolveProvider("claude", "model-y"); err != nil {
		t.Fatal(err)
	}
	var second atomic.Int64
	replacement := &stubProvider{name: "claude", available: true, response: "v2"}
	if err := r.RegisterProviderFactory("claude", func(string) (Provider, error) {
		second.Add(1)
		return replacement, nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := r.cloneWithMode(ModeFast).resolveProvider("claude", "model-y")
	if err != nil {
		t.Fatal(err)
	}
	if p != replacement || second.Load() != 1 {
		t.Fatalf("after re-registration resolved %v (new factory calls %d); want the new factory's instance", p, second.Load())
	}
}
