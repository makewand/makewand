package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/makewand/makewand/router"
)

// go-server-1021-1: with only an OpenAI key under allow_paid, Power mode's
// codex generator is built by the codex factory. It sent the codex table's
// "codex-cli" placeholder upstream and recorded the call at $0 under the zero
// (free) access type, so Power requests bypassed every USD budget. The route
// must call the configured OpenAI model and be billed as API spend.
func TestCodexRouteWithoutCLICallsAndBillsTheConfiguredOpenAIModel(t *testing.T) {
	var mu sync.Mutex
	var models []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		models = append(models, req.Model)
		mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"content":"candidate"}}],"usage":{"prompt_tokens":1000,"completion_tokens":2000}}`)
	}))
	defer upstream.Close()

	t.Setenv("MAKEWAND_API_POLICY", "allow_paid")
	cfg := policyConfig(t)
	cfg.ClaudeAPIKey = ""
	cfg.GeminiAPIKey = ""
	cfg.OpenAIBaseURL = upstream.URL + "/v1"
	cfg.OpenAIModel = "gpt-4o-mini"
	cfg.UsageMode = "power"
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := router.EstimateCost("gpt-4o-mini", 1000, 2000)
	if want <= 0 {
		t.Fatalf("fixture model is unpriced: %v", want)
	}
	messages := []Message{{Role: "user", Content: "write a function"}}
	for _, phase := range []BuildPhase{PhaseCode, PhaseFix} {
		mu.Lock()
		models = nil
		mu.Unlock()
		content, usage, result, err := r.ChatBest(context.Background(), phase, messages, "")
		if err != nil || content != "candidate" {
			t.Fatalf("phase %v: content=%q err=%v", phase, content, err)
		}
		mu.Lock()
		got := append([]string(nil), models...)
		mu.Unlock()
		if len(got) != 1 || got[0] != "gpt-4o-mini" {
			t.Fatalf("phase %v: upstream models = %q, want the configured [gpt-4o-mini]", phase, got)
		}
		if result.Actual != "codex" || result.ModelID != "gpt-4o-mini" || usage.Model != "gpt-4o-mini" {
			t.Fatalf("phase %v: route %s/%s usage model %q, want codex served by gpt-4o-mini", phase, result.Actual, result.ModelID, usage.Model)
		}
		if math.Abs(usage.Cost-want) > 1e-12 || !usage.MeasuredCost {
			t.Fatalf("phase %v: cost $%v measured=%t, want $%v at gpt-4o-mini's price", phase, usage.Cost, usage.MeasuredCost, want)
		}
	}
	// Spend the transport does not report (streams) is estimated only for API
	// access, so the never-registered codex name must carry it.
	estimate := r.EstimateUsageForRoute(router.RouteResult{Actual: "codex", ModelID: "gpt-4o-mini"}, messages, "", "an unreported answer")
	if estimate.Cost <= 0 {
		t.Fatalf("codex route estimated at $%v, want API pricing", estimate.Cost)
	}
}
