package router

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/execution"
)

func TestAgyAdapterUsesCanonicalLeaseNamespace(t *testing.T) {
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	ctx := execution.ContextWithConfig(t.Context(), execution.Config{LedgerPath: filepath.Join(t.TempDir(), "calls.json"), TaskID: "agy-alias", MaxCalls: 10})
	a, err := execution.Reserve(ctx, execution.Metadata{Engine: "agy"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Complete(execution.Outcome{Status: execution.Unknown})
	if _, err := execution.Reserve(ctx, execution.Metadata{Engine: NewAgyCLI("fake-unused").Name()}); !errors.Is(err, execution.ErrConcurrencyBusy) {
		t.Fatalf("actual adapter bypasses agy capacity: %v", err)
	}
}

type forbiddenAPILeaseTransport struct{ t *testing.T }

func (f forbiddenAPILeaseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("HTTP transport started despite occupied API capacity")
	return nil, errors.New("fixture transport must not run")
}

func TestNativeGeminiAPICompetesWithAgyAPILeaseBeforeTransport(t *testing.T) {
	t.Setenv("MAKEWAND_LAYERED_BUDGET", "1")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	ctx := execution.ContextWithConfig(t.Context(), execution.Config{
		LedgerPath: filepath.Join(t.TempDir(), "calls.json"), TaskID: "native-api", MaxCalls: 10,
	})
	account := "agy:api:fixture-key"
	a, err := execution.Reserve(execution.ContextWithConfig(ctx, execution.Config{AccountRef: &account}), execution.Metadata{Engine: "agy"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Complete(execution.Outcome{Status: execution.Unknown})
	provider := NewGeminiWithBaseURL("fixture-key", "fixture-model", "https://fixture.invalid")
	provider.chatClient = &http.Client{Transport: forbiddenAPILeaseTransport{t}}
	provider.streamClient = provider.chatClient
	if _, _, err := provider.Chat(ctx, nil, "", 16); !errors.Is(err, execution.ErrConcurrencyBusy) {
		t.Fatalf("native API bypassed agy lease: %v", err)
	}
	if _, err := provider.ChatStream(ctx, nil, "", 16); !errors.Is(err, execution.ErrConcurrencyBusy) {
		t.Fatalf("native streaming API bypassed agy lease: %v", err)
	}
	_, subscription, err := reserveProvider(ctx, NewAgyCLI("fake-unused"))
	if err != nil {
		t.Fatalf("API capacity occupied subscription: %v", err)
	}
	defer subscription.Complete(execution.Outcome{Status: execution.Unknown})
}

func TestBuiltinAPIAdmissionDoesNotBorrowSubscriptionIdentity(t *testing.T) {
	for _, provider := range []Provider{NewOpenAI("fixture-key", "fixture-model"), NewClaude("fixture-key", "fixture-model"), NewGemini("fixture-key", "fixture-model")} {
		t.Run(provider.Name(), func(t *testing.T) {
			account := "claude:account:subscription-fixture"
			ctx := execution.ContextWithConfig(t.Context(), execution.Config{AccountRef: &account})
			admitted, attempt, err := reserveProvider(ctx, provider)
			if err != nil {
				t.Fatal(err)
			}
			defer attempt.Complete(execution.Outcome{Status: execution.Unknown})
			_, cfg, err := execution.EnsureContext(admitted)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AccountRef == nil || *cfg.AccountRef != provider.Name()+":api:unknown" {
				t.Fatalf("API borrowed subscription identity: %+v", cfg.AccountRef)
			}
			if admitted.Value(credentialBindingKey{}) != nil {
				t.Fatal("API admission read a subscription credential snapshot")
			}
		})
	}
}
