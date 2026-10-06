package router

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/makewand/makewand/internal/testfixture"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/makewand/makewand/execution"
)

type dispatchBudgetProvider struct {
	name  string
	calls *atomic.Int32
	err   error
}

func (p *dispatchBudgetProvider) Name() string      { return p.name }
func (p *dispatchBudgetProvider) IsAvailable() bool { return true }
func (p *dispatchBudgetProvider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	p.calls.Add(1)
	return "synthetic output", Usage{Provider: p.name}, p.err
}
func (p *dispatchBudgetProvider) ChatStream(context.Context, []Message, string, int) (<-chan StreamChunk, error) {
	p.calls.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	return bufferedStream(StreamChunk{Content: "synthetic stream", Done: true}), nil
}

func budgetTestContext(t *testing.T, maximum int) (context.Context, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.json")
	return execution.ContextWithConfig(context.Background(), execution.Config{LedgerPath: path, MaxCalls: maximum, TaskID: "router-budget-test"}), path
}
func budgetTestAttempts(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		Attempts []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	return ledger.Attempts
}

func TestRouterNativeDispatchesShareAtomicHardLimit(t *testing.T) {
	ctx, path := budgetTestContext(t, 5)
	var calls atomic.Int32
	p := &dispatchBudgetProvider{name: "claude", calls: &calls}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: p, Access: AccessSubscription}}, DefaultModel: "claude"})
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			var err error
			switch i % 3 {
			case 0:
				_, _, _, err = r.Chat(ctx, TaskCode, nil, "")
			case 1:
				_, _, _, err = r.ChatWith(ctx, "claude", PhaseCode, nil, "")
			case 2:
				var stream <-chan StreamChunk
				stream, _, err = r.ChatStream(ctx, TaskCode, nil, "")
				if err == nil {
					for chunk := range stream {
						if chunk.Error != nil {
							err = chunk.Error
						}
					}
				}
			}
			if err != nil && !errors.Is(err, execution.ErrBudgetExhausted) {
				t.Errorf("dispatch: %v", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 5 || len(budgetTestAttempts(t, path)) != 5 {
		t.Fatalf("native dispatch count=%d", calls.Load())
	}
}

func TestRouterUnknownOutcomeStopsFallbackAndKeepsSlot(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		ctx, path := budgetTestContext(t, 2)
		var primaryCalls, fallbackCalls atomic.Int32
		primary := &dispatchBudgetProvider{name: "claude", calls: &primaryCalls, err: newProviderError("claude", "dispatch", ErrorKindNetwork, true, 0, "connection reset", nil)}
		fallback := &dispatchBudgetProvider{name: "gemini", calls: &fallbackCalls}
		r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: primary, Access: AccessSubscription}, "gemini": {Provider: fallback, Access: AccessSubscription}}, DefaultModel: "claude", CodingModel: "claude"})
		var err error
		if streaming {
			_, _, err = r.ChatStream(ctx, TaskCode, nil, "")
		} else {
			_, _, _, err = r.Chat(ctx, TaskCode, nil, "")
		}
		if !errors.Is(err, execution.ErrUnknownOutcome) || primaryCalls.Load() != 1 || fallbackCalls.Load() != 0 {
			t.Fatalf("stream=%v err=%v calls=%d/%d", streaming, err, primaryCalls.Load(), fallbackCalls.Load())
		}
		attempts := budgetTestAttempts(t, path)
		if len(attempts) != 1 || attempts[0]["result_status"] != "UNKNOWN" || attempts[0]["outcome_known"] != false || attempts[0]["tokens"] != nil || attempts[0]["monetary_cost"] != nil {
			t.Fatalf("unknown result lost: %+v", attempts)
		}
	}
}

func TestPowerUnknownGenerationIsNotReplayedThroughAdaptiveFallback(t *testing.T) {
	ctx, path := budgetTestContext(t, 10)
	var calls atomic.Int32
	providers := map[string]ProviderEntry{}
	for _, name := range []string{"claude", "gemini", "codex"} {
		providers[name] = ProviderEntry{Provider: &dispatchBudgetProvider{name: name, calls: &calls, err: newProviderError(name, "dispatch", ErrorKindNetwork, true, 0, "synthetic lost result", nil)}, Access: AccessSubscription}
	}
	r := mustNewRouter(RouterConfig{Providers: providers, UsageMode: "power"})
	if _, _, _, err := r.ChatBest(ctx, PhaseCode, nil, ""); !errors.Is(err, execution.ErrUnknownOutcome) {
		t.Fatalf("power hid unknown generation: %v", err)
	}
	attempts := budgetTestAttempts(t, path)
	ensemble, _ := r.routingTables().powerEnsembleFor(PhaseCode)
	expected := len(ensemble.Generators)
	if int(calls.Load()) != expected || len(attempts) != expected {
		t.Fatalf("power replayed failed generators: calls=%d attempts=%d", calls.Load(), len(attempts))
	}
	for _, attempt := range attempts {
		if attempt["task_id"] != "router-budget-test" || attempt["result_status"] != "UNKNOWN" {
			t.Fatalf("power outcome lost: %+v", attempt)
		}
	}
}

func TestBuiltinMalformedResponseIsUnknownAndDoesNotFallback(t *testing.T) {
	ctx, path := budgetTestContext(t, 2)
	var requests, fallbackCalls atomic.Int32
	p := NewOpenAI("synthetic-test-key", "test-model")
	p.chatClient = newMockClient(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); _, _ = io.WriteString(w, `{"choices":`) })
	fallback := &dispatchBudgetProvider{name: "claude", calls: &fallbackCalls}
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: p, Access: AccessAPI}, "claude": {Provider: fallback, Access: AccessSubscription}}, DefaultModel: "openai", CodingModel: "openai"})
	if _, _, _, err := r.Chat(ctx, TaskCode, nil, ""); !errors.Is(err, execution.ErrUnknownOutcome) {
		t.Fatalf("malformed accepted response known: %v", err)
	}
	if requests.Load() != 1 || fallbackCalls.Load() != 0 || len(budgetTestAttempts(t, path)) != 1 {
		t.Fatal("malformed response replayed")
	}
}

func TestBuiltinDirectAPIAdmissionDoesNotDoubleCountRouter(t *testing.T) {
	ctx, path := budgetTestContext(t, 1)
	var requests atomic.Int32
	p := NewOpenAI("synthetic-test-key", "test-model")
	p.chatClient = newMockClient(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"mock"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
	r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: p, Access: AccessAPI}}, DefaultModel: "openai", ExecutionAPIPolicy: "allow_paid"})
	if _, _, _, err := r.Chat(ctx, TaskExplain, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Chat(ctx, nil, "", 100); !errors.Is(err, execution.ErrBudgetExhausted) {
		t.Fatalf("direct provider bypassed gate: %v", err)
	}
	attempts := budgetTestAttempts(t, path)
	if requests.Load() != 1 || len(attempts) != 1 || attempts[0]["tokens"] != float64(15) || attempts[0]["monetary_cost"] != nil || attempts[0]["api_policy"] != "allow_paid" {
		t.Fatalf("incorrect dispatch measurement: requests=%d %+v", requests.Load(), attempts)
	}
}

func countingSyntheticCLI(t *testing.T, firstError string) (*CLIProvider, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "count")
	script := testfixture.WriteCLI(t, "synthetic", testfixture.Spec{Mode: "counter", StateFile: state, FirstError: firstError, Stdout: "synthetic response\n"})
	return NewCommandCLI("synthetic", script, []string{state}, PromptModeStdin), state
}

func TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry(t *testing.T) {
	for _, test := range []struct {
		first         string
		maximum, want int
		unknown       bool
	}{{"quota exceeded", 1, 1, false}, {"quota exceeded", 2, 2, false}, {"stream closed unexpectedly: Transport error (1007)", 2, 1, true}} {
		ctx, path := budgetTestContext(t, test.maximum)
		p, state := countingSyntheticCLI(t, test.first)
		_, _, err := ChatProvider(ctx, p, nil, "", 100)
		if test.unknown && !errors.Is(err, execution.ErrUnknownOutcome) {
			t.Fatalf("unknown replayed: %v", err)
		}
		if !test.unknown && test.maximum == 1 && !errors.Is(err, execution.ErrBudgetExhausted) {
			t.Fatalf("retry bypassed cap: %v", err)
		}
		if test.maximum == 2 && !test.unknown && err != nil {
			t.Fatal(err)
		}
		count, readErr := os.ReadFile(state)
		if readErr != nil {
			t.Fatal(readErr)
		}
		n, parseErr := strconv.Atoi(strings.TrimSpace(string(count)))
		if parseErr != nil || n != test.want || len(budgetTestAttempts(t, path)) != test.want {
			t.Fatalf("process/ledger count=%s /%d; expected %d", count, len(budgetTestAttempts(t, path)), test.want)
		}
	}
}

func TestExecutionTelemetryFailureCannotReplaySuccessfulDispatch(t *testing.T) {
	ctx, path := budgetTestContext(t, 1)
	_, cfg, err := execution.EnsureContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.EventsPath = t.TempDir() // A directory cannot be opened as a JSONL file.
	var warnings, calls atomic.Int32
	cfg.EventError = func(error) { warnings.Add(1) }
	ctx = execution.ContextWithConfig(ctx, cfg)
	p := &dispatchBudgetProvider{name: "claude", calls: &calls}
	if _, _, err := ChatProvider(ctx, p, nil, "", 100); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || warnings.Load() != 2 || budgetTestAttempts(t, path)[0]["result_status"] != "PASSED" {
		t.Fatal("telemetry changed successful accounting")
	}
}
