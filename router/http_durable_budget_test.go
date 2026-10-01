package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

type billedFailureProvider struct {
	stubProvider
	cost     float64
	failure  error
	measured bool
	before   func() error
}

func (p *billedFailureProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	if p.before != nil {
		if err := p.before(); err != nil {
			return "", Usage{}, err
		}
	}
	return "", Usage{Provider: p.name, Cost: p.cost, MeasuredCost: p.measured}, p.failure
}

func (p *billedFailureProvider) ChatStream(context.Context, []Message, string, int) (<-chan StreamChunk, error) {
	if p.before != nil {
		if err := p.before(); err != nil {
			return nil, err
		}
	}
	return nil, p.failure
}

func TestRouterRetainsFailedProviderCostOnFallback(t *testing.T) {
	first := &billedFailureProvider{stubProvider: stubProvider{name: "claude", available: true}, cost: .03, failure: errors.New("paid failure"), measured: true}
	last := &stubProvider{name: "gemini", available: true, response: "fallback", cost: .02}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: first, Access: AccessAPI}, "gemini": {Provider: last, Access: AccessAPI}}, DefaultModel: "claude"})
	content, usage, _, err := r.ChatWith(context.Background(), "claude", PhaseCode, []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil || content != "fallback" || usage.Cost != .05 {
		t.Fatalf("failed attempt spend lost: content=%s usage=%+v err=%v", content, usage, err)
	}
}

func TestHTTPDurableUnknownCostCannotBeRefunded(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct {
				name    string
				corrupt bool
				failure error
			}{
				{"normal_unknown", false, &execution.UnknownOutcomeError{Err: errors.New("response lost")}},
				{"completion_ledger_corrupted_unknown", true, &execution.UnknownOutcomeError{Err: errors.New("response lost")}},
				{"completion_ledger_corrupted_timeout", true, context.DeadlineExceeded},
			} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", endpoint, streaming, tc.name), func(t *testing.T) {
					store, err := serverusage.OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer store.Close()
					auth, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{ID: "t", Token: "secret", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerMonth: .01}}})
					if err != nil {
						t.Fatal(err)
					}
					provider := &billedFailureProvider{stubProvider: stubProvider{name: "claude", available: true}, failure: tc.failure}
					ctx := context.Background()
					if tc.corrupt {
						var path string
						ctx, path = budgetTestContext(t, 2)
						provider.before = func() error { return os.WriteFile(path, []byte("injected corrupt completion ledger"), 0o600) }
					}
					r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: provider, Access: AccessAPI}}, DefaultModel: "claude"})
					handler := r.HTTPHandler(HTTPHandlerOptions{Authorizer: auth, UsageLogger: store, UsageReader: store, BudgetReserver: store, BudgetReservationUSD: .01, StrictAccounting: true})
					invoke := func() int {
						payload := fmt.Sprintf(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"stream":%t}`, streaming)
						if endpoint == "/v1/responses" {
							payload = fmt.Sprintf(`{"model":"claude","input":"hi","stream":%t}`, streaming)
						}
						request := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(payload))
						request = request.WithContext(ctx)
						request.Header.Set("Authorization", "Bearer secret")
						response := httptest.NewRecorder()
						handler.ServeHTTP(response, request)
						return response.Code
					}
					if status := invoke(); status != http.StatusServiceUnavailable {
						t.Fatalf("unknown response status=%d", status)
					}
					entries, err := store.Load(serverusage.Filter{TokenID: "t"})
					if err != nil || len(entries) != 1 || !entries[0].EstimatedCost || entries[0].CostUSD != .01 {
						t.Fatalf("unknown billing improperly refunded: %+v err=%v", entries, err)
					}
					if status := invoke(); status != http.StatusTooManyRequests {
						t.Fatalf("unknown request credit restored: %d", status)
					}
				})
			}
		}
	}
}
