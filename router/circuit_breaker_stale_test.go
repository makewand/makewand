package router

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestBreakerStaleSuccessDoesNotCloseOpenCircuit is the unit repro of
// go-router#6: a success that was not the half-open probe (here an unticketed
// success, i.e. one admitted before the circuit opened) closed the circuit.
func TestBreakerStaleSuccessDoesNotCloseOpenCircuit(t *testing.T) {
	cb := newProviderCircuitBreaker(3, 30*time.Second)
	if opened, _ := cb.RecordFailureWeighted("p", 3); !opened {
		t.Fatal("weighted failure should open the circuit")
	}
	cb.RecordSuccess("p")
	if open, _ := cb.PeekOpen("p"); !open {
		t.Fatal("a success that was not the half-open probe closed the open circuit")
	}
}

// gatedProvider blocks "slow" requests until released so a test can hold one
// request in flight while another trips the breaker.
type gatedProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gatedProvider) Name() string      { return "claude" }
func (g *gatedProvider) IsAvailable() bool { return true }
func (g *gatedProvider) Chat(ctx context.Context, messages []Message, _ string, _ int) (string, Usage, error) {
	if len(messages) > 0 && messages[len(messages)-1].Content == "slow" {
		g.entered <- struct{}{}
		select {
		case <-g.release:
			return "late ok", Usage{Provider: "claude"}, nil
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	}
	return "", Usage{}, errors.New("upstream timed out")
}
func (g *gatedProvider) ChatStream(context.Context, []Message, string, int) (<-chan StreamChunk, error) {
	return nil, errors.New("not used")
}

// TestInFlightSuccessAfterTimeoutTripKeepsCircuitOpen is the router-level
// repro: request B is admitted while the circuit is closed; request A then
// times out and trips the breaker; B's late success must not close it.
func TestInFlightSuccessAfterTimeoutTripKeepsCircuitOpen(t *testing.T) {
	g := &gatedProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	r := mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{"claude": {Provider: g, Access: AccessSubscription}},
		UsageMode: "balanced",
	})

	done := make(chan error, 1)
	go func() {
		_, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "slow"}}, "")
		done <- err
	}()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request never reached the provider")
	}

	if _, _, _, err := r.Chat(context.Background(), TaskCode, []Message{{Role: "user", Content: "fast-timeout"}}, ""); err == nil {
		t.Fatal("timeout request unexpectedly succeeded")
	}
	if open, _ := r.isCircuitOpen("claude"); !open {
		t.Fatal("precondition: a timeout should trip the breaker immediately")
	}

	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("slow request: %v", err)
	}
	if open, _ := r.isCircuitOpen("claude"); !open {
		t.Fatal("the late success of a request admitted before the trip closed the circuit; only the half-open probe may close it")
	}
}

// TestBreakerOnlyCurrentProbeClosesHalfOpenCircuit pins the ticket semantics:
// a stale success during half-open neither closes the circuit nor releases the
// probe slot; the current probe's success does close it.
func TestBreakerOnlyCurrentProbeClosesHalfOpenCircuit(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cb := newProviderCircuitBreaker(3, 10*time.Second)
	cb.now = func() time.Time { return now }

	_, _, stale := cb.Admit("p") // admitted while closed
	if opened, _ := cb.RecordFailureWeighted("p", 3); !opened {
		t.Fatal("weighted failure should open the circuit")
	}
	cb.RecordAdmittedSuccess("p", stale)
	if open, _ := cb.PeekOpen("p"); !open {
		t.Fatal("stale success closed the cooling-down circuit")
	}

	now = now.Add(11 * time.Second)
	allow, _, probe := cb.Admit("p")
	if !allow || !probe.probe {
		t.Fatalf("cooldown expiry should admit exactly one probe, got allow=%v ticket=%+v", allow, probe)
	}
	cb.RecordAdmittedSuccess("p", stale)
	if ok, _ := cb.BeforeAttempt("p"); ok {
		t.Fatal("stale success during half-open released the probe slot / closed the circuit")
	}

	// A probe from an earlier open episode is stale too.
	if opened, _ := cb.RecordFailure("p"); !opened {
		t.Fatal("probe failure should re-open")
	}
	now = now.Add(11 * time.Second)
	allow, _, probe2 := cb.Admit("p")
	if !allow || !probe2.probe {
		t.Fatal("second cooldown expiry should admit a new probe")
	}
	cb.RecordAdmittedSuccess("p", probe)
	if ok, _ := cb.BeforeAttempt("p"); ok {
		t.Fatal("a previous episode's probe success closed the circuit")
	}

	cb.RecordAdmittedSuccess("p", probe2)
	if ok, _ := cb.BeforeAttempt("p"); !ok {
		t.Fatal("the current probe's success should close the circuit")
	}
}

// TestBreakerClosedSuccessResetsFailureStreak keeps the closed-state behavior:
// a success resets the consecutive-failure count.
func TestBreakerClosedSuccessResetsFailureStreak(t *testing.T) {
	cb := newProviderCircuitBreaker(3, 10*time.Second)
	cb.RecordFailure("p")
	cb.RecordFailure("p")
	_, _, ticket := cb.Admit("p")
	cb.RecordAdmittedSuccess("p", ticket)
	if opened, _ := cb.RecordFailure("p"); opened {
		t.Fatal("success in the closed state should have reset the failure streak")
	}
}
