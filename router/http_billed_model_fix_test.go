package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

// billingUpstream is a loopback fake of the OpenAI chat-completions and
// Anthropic messages APIs. Non-streaming replies report usage (1000 prompt and
// 2000 completion tokens); streams carry none, like most gateways, and send
// one-token words ("go "), wordsPerEvent per SSE event: exactly the request's
// max_tokens of them, or overrun times as many for a provider that ignores its
// output cap. dense streams CJK runes without spaces instead of words.
type billingUpstream struct {
	*httptest.Server
	wordsPerEvent int
	overrun       int
	dense         bool

	mu        sync.Mutex
	models    []string
	maxTokens []int
}

func newBillingUpstream(t *testing.T, wordsPerEvent, overrun int, dense bool) *billingUpstream {
	t.Helper()
	u := &billingUpstream{wordsPerEvent: wordsPerEvent, overrun: overrun, dense: dense}
	u.Server = httptest.NewServer(u)
	t.Cleanup(u.Close)
	return u
}

func (u *billingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.models = append(u.models, req.Model)
	u.maxTokens = append(u.maxTokens, req.MaxTokens)
	u.mu.Unlock()
	anthropic := strings.HasSuffix(r.URL.Path, "/messages")
	if !req.Stream {
		if anthropic {
			fmt.Fprint(w, `{"content":[{"text":"ok"}],"usage":{"input_tokens":1000,"output_tokens":2000}}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":2000}}`)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	total := req.MaxTokens * max(1, u.overrun)
	for sent := 0; sent < total && r.Context().Err() == nil; sent += u.wordsPerEvent {
		unit := "go "
		if u.dense {
			unit = "中"
		}
		text, _ := json.Marshal(strings.Repeat(unit, min(u.wordsPerEvent, total-sent)))
		if anthropic {
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", text)
		} else {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", text)
		}
	}
	if anthropic {
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	} else {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func (u *billingUpstream) requests() (models []string, maxTokens []int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...), append([]int(nil), u.maxTokens...)
}

// billingLedger is a durable SQLite usage store (also the budget reserver)
// that keeps each entry as the handler logged it: billing_uncertain is not a
// durable column, it only decides how the row settles.
type billingLedger struct {
	*serverusage.SQLiteStore
	mu     sync.Mutex
	logged []serverusage.Entry
}

func (l *billingLedger) Log(e serverusage.Entry) error {
	l.mu.Lock()
	l.logged = append(l.logged, e)
	l.mu.Unlock()
	return l.SQLiteStore.Log(e)
}

func (l *billingLedger) loggedEntry(t *testing.T, stream bool) serverusage.Entry {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []serverusage.Entry
	for _, e := range l.logged {
		if e.Stream == stream {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("stream=%t logged entries=%+v", stream, l.logged)
	}
	return found[0]
}

// billingHandler serves r with a billingLedger and a generous daily cap,
// optionally with scaled reservations.
func billingHandler(t *testing.T, r *Router, scaled bool) (http.Handler, *billingLedger) {
	t.Helper()
	store, err := serverusage.OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ledger := &billingLedger{SQLiteStore: store}
	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{ID: "t", Token: "secret", Scopes: []string{serverauth.ScopeChatInvoke}, MaxCostUSDPerDay: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	return r.HTTPHandler(HTTPHandlerOptions{Authorizer: authz, UsageLogger: ledger, UsageReader: store, BudgetReserver: store,
		BudgetReservationUSD: 0.0001, ScaleBudgetReservation: scaled}), ledger
}

func billingChat(t *testing.T, h http.Handler, model, prompt string, stream bool) string {
	t.Helper()
	body := map[string]any{"stream": stream, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	if model != "" {
		body["model"] = model
	}
	payload, _ := json.Marshal(body)
	rec := postFix16Chat(t, h, "secret", string(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("model=%q stream=%t status=%d body=%s", model, stream, rec.Code, reservationTail(rec.Body.String(), 300))
	}
	return rec.Body.String()
}

// sseDeltas returns the streamed content and the model the chunks report.
func sseDeltas(t *testing.T, body string) (content, model string) {
	t.Helper()
	var b strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad SSE chunk %q: %v", data, err)
		}
		if model == "" {
			model = chunk.Model
		}
		for _, choice := range chunk.Choices {
			b.WriteString(choice.Delta.Content)
		}
	}
	return b.String(), model
}

// onlyEntry returns the durable (settled) row of the one stream or
// non-stream request.
func onlyEntry(t *testing.T, store *billingLedger, stream bool) serverusage.Entry {
	t.Helper()
	entries, err := store.Load(serverusage.Filter{TokenID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var found []serverusage.Entry
	for _, e := range entries {
		if e.Stream == stream {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("stream=%t entries=%+v", stream, entries)
	}
	return found[0]
}

func tablePrice(t *testing.T, modelID string) costEntry {
	t.Helper()
	price, ok := defaultTables.costFor(modelID)
	if !ok {
		t.Fatalf("%s has no pricing entry", modelID)
	}
	return price
}

func billedAt(e serverusage.Entry, price costEntry) float64 {
	return (float64(e.PromptTokens)*price.Input + float64(e.CompletionTokens)*price.Output) / 1_000_000
}

// cliFailoverRouter mirrors `makewand serve` with the claude CLI installed and
// an Anthropic key: "claude" is the subscription CLI (failing here) and the
// "claude-api" sibling calls the configured model.
func cliFailoverRouter(model, baseURL string) *Router {
	return mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"claude":     {Provider: &stubProvider{name: "claude", available: true, failChat: true, failStream: true}, Access: AccessSubscription},
			"claude-api": {Provider: NewClaudeWithBaseURL("fixture-key", model, baseURL), Access: AccessAPI},
		},
		DefaultModel: "claude",
		CodingModel:  "claude",
	})
}

func TestAPIProvidersReportTheModelTheyCall_Fix16(t *testing.T) {
	openai := NewOpenAIWithBaseURL("k", "o1", "http://127.0.0.1:1/v1")
	claude := NewClaudeWithBaseURL("k", "claude-opus-4-20250514", "http://127.0.0.1:1")
	for _, tc := range []struct {
		provider  Provider
		requested string
		want      string
	}{
		{openai, "", "o1"},
		{openai, "gpt-4o-mini", "gpt-4o-mini"},
		{claude, "", "claude-opus-4-20250514"},
		{claude, "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
	} {
		if got := reportedModelID(tc.provider, tc.requested); got != tc.want {
			t.Errorf("%s reports %q for requested %q, want %q", tc.provider.Name(), got, tc.requested, tc.want)
		}
	}
}

// A stream through the OpenAI provider is billed at the price of its
// configured model - the model the upstream receives - not at the unpriced
// fallback rate: o1 ($15/$60) was under-billed 4x and gpt-4o-mini over-billed
// 25x. The stream also names that model, like the non-streaming reply.
func TestHTTPStreamBilledAtTheModelTheOpenAIProviderCalls_Fix16(t *testing.T) {
	for _, model := range []string{"gpt-4o-mini", "gpt-4o", "o1"} {
		t.Run(model, func(t *testing.T) {
			upstream := newBillingUpstream(t, 10, 1, false)
			r := mustNewRouter(RouterConfig{
				Providers:    map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("fixture-key", model, upstream.URL+"/v1"), Access: AccessAPI}},
				DefaultModel: "openai",
				CodingModel:  "openai",
			})
			h, store := billingHandler(t, r, false)
			_, streamModel := sseDeltas(t, billingChat(t, h, "", "explain what a monad is", true))
			plain := billingChat(t, h, "", "explain what a monad is", false)
			if models, _ := upstream.requests(); len(models) != 2 || models[0] != model || models[1] != model {
				t.Fatalf("upstream models = %q, want %q", models, model)
			}
			if streamModel != model || !strings.Contains(plain, fmt.Sprintf(`"model":%q`, model)) {
				t.Fatalf("stream reports model %q (non-stream reply %s), want %q", streamModel, reservationTail(plain, 120), model)
			}
			price := tablePrice(t, model)
			streamed := onlyEntry(t, store, true)
			if want := billedAt(streamed, price); streamed.CostUSD <= 0 || math.Abs(streamed.CostUSD-want) > 1e-9 {
				t.Fatalf("stream of %d+%d tokens billed $%v, want $%v at %s's table price", streamed.PromptTokens, streamed.CompletionTokens, streamed.CostUSD, want, model)
			}
			if nonStream := onlyEntry(t, store, false); math.Abs(nonStream.CostUSD-billedAt(nonStream, price)) > 1e-9 {
				t.Fatalf("non-stream billed $%v, want $%v", nonStream.CostUSD, billedAt(nonStream, price))
			}
		})
	}
}

// A client naming a Claude model pins the "claude" subscription CLI; when it
// fails, the "claude-api" sibling serves the request with its configured model.
// The stream is billed at that model's price, as the non-streaming call is.
func TestHTTPStreamBilledAtConfiguredModelOfClaudeAPISibling_Fix16(t *testing.T) {
	const opus = "claude-opus-4-20250514"
	upstream := newBillingUpstream(t, 10, 1, false)
	h, store := billingHandler(t, cliFailoverRouter(opus, upstream.URL), false)
	billingChat(t, h, "claude-api", "explain what a monad is", true)
	billingChat(t, h, "claude-api", "explain what a monad is", false)
	if models, _ := upstream.requests(); len(models) != 2 || models[0] != opus || models[1] != opus {
		t.Fatalf("upstream models = %q, want %q", models, opus)
	}
	price := tablePrice(t, opus)
	for _, stream := range []bool{true, false} {
		e := onlyEntry(t, store, stream)
		if want := billedAt(e, price); math.Abs(e.CostUSD-want) > 1e-9 {
			t.Fatalf("stream=%t: %d+%d tokens billed $%v, want $%v at opus price", stream, e.PromptTokens, e.CompletionTokens, e.CostUSD, want)
		}
	}
}

// The reservation ceiling covers exactly the models routing can bill - the
// configured model of a provider without a model table entry, and every model
// of a pinned provider's fallbacks - so it is never below the price a stream
// is billed and guarded at.
func TestHTTPPriceCeilingCoversTheModelsProvidersCall_Fix16(t *testing.T) {
	openaiRouter := func(model string) *Router {
		return mustNewRouter(RouterConfig{
			Providers:    map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", model, "http://127.0.0.1:1/v1"), Access: AccessAPI}},
			DefaultModel: "openai",
		})
	}
	opus := tablePrice(t, "claude-opus-4-20250514")
	for _, tc := range []struct {
		name   string
		router *Router
		want   costEntry
	}{
		{"openai o1", openaiRouter("o1"), tablePrice(t, "o1")},
		{"openai gpt-4o-mini", openaiRouter("gpt-4o-mini"), tablePrice(t, "gpt-4o-mini")},
		{"openai unpriced", openaiRouter("gpt-unpriced-fixture"), unpricedFallbackCost()},
		// claude-api calls its configured haiku when pinned, and the base
		// provider's tier models when legacy or candidate routing reaches it.
		{"claude-api haiku", cliFailoverRouter("claude-haiku-4-5-20251001", "http://127.0.0.1:1"), opus},
		// A request pinned to the "claude" CLI, which has no price, falls back
		// to its claude-api sibling, which has one.
		{"claude CLI with an API sibling", cliFailoverRouter("claude-opus-4-20250514", "http://127.0.0.1:1"), opus},
	} {
		got, ok := tc.router.httpPriceCeiling()
		if !ok || got != tc.want {
			t.Errorf("%s: ceiling = %+v ok=%v, want %+v", tc.name, got, ok, tc.want)
		}
	}
}

// A reply that stays within the provider call's max_tokens must never be cut
// by the scaled-reservation spend guard - not on an unpriced route (route
// price = ceiling = fallback), not on a route whose model is the ceiling, not
// on a pinned CLI request served by its API sibling - however the upstream
// batches its tokens, and its row settles at its estimate (capped at
// max_tokens), not the reservation.
func TestHTTPStreamGuardNeverCutsRepliesWithinMaxTokens_Fix16(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		router      func(baseURL string) *Router
	}{
		{name: "openai unpriced", router: func(u string) *Router {
			return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-unpriced-fixture", u+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
		}},
		{name: "openai o1", router: func(u string) *Router {
			return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "o1", u+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
		}},
		{name: "pinned claude CLI to claude-api", model: "claude-sonnet", router: func(u string) *Router { return cliFailoverRouter("claude-opus-4-20250514", u) }},
	} {
		for _, prompt := range []string{"write a long story please", "explain what a monad is", "explain lorem"} {
			for _, wordsPerEvent := range []int{1, 10, 37} {
				t.Run(fmt.Sprintf("%s/%s/%d", tc.name, prompt, wordsPerEvent), func(t *testing.T) {
					upstream := newBillingUpstream(t, wordsPerEvent, 1, false)
					h, store := billingHandler(t, tc.router(upstream.URL), true)
					body := billingChat(t, h, tc.model, prompt, true)
					content, _ := sseDeltas(t, body)
					_, maxTokens := upstream.requests()
					if strings.Contains(body, "budget_reservation_exceeded") || len(maxTokens) == 0 {
						t.Fatalf("reply within max_tokens=%v was cut after %d words; tail=%q", maxTokens, strings.Count(content, "go"), reservationTail(body, 200))
					}
					if words := strings.Count(content, "go"); words != maxTokens[len(maxTokens)-1] {
						t.Fatalf("delivered %d words, want all %d", words, maxTokens[len(maxTokens)-1])
					}
					if logged := store.loggedEntry(t, true); logged.UncertainCost {
						t.Fatalf("complete reply logged with an uncertain cost: %+v", logged)
					}
					want := min(estimateTokens(content), maxTokens[len(maxTokens)-1])
					if e := onlyEntry(t, store, true); !e.EstimatedCost || e.CompletionTokens != want {
						t.Fatalf("row = %+v, want an estimated cost for all %d completion tokens", e, want)
					}
				})
			}
		}
	}
}

// The guard still stops streams that ignore their output cap, including text
// without spaces, and only after they pass what the reservation covers.
func TestHTTPStreamGuardStillStopsRunaways_Fix16(t *testing.T) {
	for _, dense := range []bool{false, true} {
		t.Run(fmt.Sprintf("dense=%t", dense), func(t *testing.T) {
			upstream := newBillingUpstream(t, 40, 8, dense)
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-unpriced-fixture", upstream.URL+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
			h, store := billingHandler(t, r, true)
			body := billingChat(t, h, "", "explain what a monad is", true)
			content, _ := sseDeltas(t, body)
			_, maxTokens := upstream.requests()
			units := strings.Count(content, "go") + strings.Count(content, "中")
			if !strings.Contains(body, "budget_reservation_exceeded") {
				t.Fatalf("runaway of %d units (max_tokens=%v) was not stopped", units, maxTokens)
			}
			if len(maxTokens) != 1 || units <= maxTokens[0] || units >= 8*maxTokens[0] {
				t.Fatalf("runaway stopped after %d units, want after max_tokens=%v and before the end", units, maxTokens)
			}
			if logged := store.loggedEntry(t, true); !logged.UncertainCost || !logged.EstimatedCost {
				t.Fatalf("stopped runaway logged as %+v, want an uncertain estimated cost", logged)
			}
		})
	}
}

// A reply of exactly the covered size fits: the reservation and the guard sum
// the same prices in a different order, which must not cut the last token.
func TestHTTPStreamGuardBoundaryIsExact_Fix16(t *testing.T) {
	r := unpricedOpenAIRouter(t, "http://127.0.0.1:1")
	msgs := []Message{{Role: "user", Content: "explain lorem"}}
	prepared := &preparedHTTPChat{Router: r, Task: TaskExplain, Messages: msgs}
	guard := newHTTPStreamSpendGuard(r, r.httpReservationUSD(0.0001, prepared), RouteResult{Actual: "openai"}, msgs, "", Usage{})
	maxOut := MaxTokensForTask(TaskExplain)
	for i := 1; i <= maxOut; i++ {
		if guard.add("go ") {
			t.Fatalf("reply cut at token %d of max_tokens %d", i, maxOut)
		}
	}
	if !guard.add("go ") {
		t.Fatalf("token %d past max_tokens %d at the ceiling price was not cut", maxOut+1, maxOut)
	}
}

func TestStreamTokenFloor_Fix16(t *testing.T) {
	floor := func(chunks ...string) int {
		var f streamTokenFloor
		for _, chunk := range chunks {
			f.add(chunk)
		}
		return f.tokens()
	}
	// split cuts text into chunks of size runes: decoded SSE content always
	// ends on a rune boundary.
	split := func(text string, size int) []string {
		var chunks []string
		runes := []rune(text)
		for len(runes) > size {
			chunks = append(chunks, string(runes[:size]))
			runes = runes[size:]
		}
		return append(chunks, string(runes))
	}
	for _, tc := range []struct {
		name   string
		chunks []string
		want   int
	}{
		{"empty", nil, 0},
		{"whitespace only", []string{" \n\t　 "}, 0},
		{"one-token words", split(strings.Repeat("go ", 100), 7), 100},
		{"word split across chunks counts once", []string{"imple", "mentation", " done"}, 2},
		{"word in progress counts", []string{"implementation"}, 1},
		{"long word: one token per 8 characters past the first 16", []string{strings.Repeat("x", 160)}, 19},
		{"long word streamed in pieces", split(strings.Repeat("x", 161), 3), 20},
		{"dense script: one token per 4 runes past the first 8", split(strings.Repeat("中", 40), 5), 9},
		{"mixed", []string{"Tokenizers\tnever\nmerge 中文中文中文 across　spaces."}, 6},
	} {
		if got := floor(tc.chunks...); got != tc.want {
			t.Errorf("%s: floor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Durable rows flag a cost no transport measured: an unpriced model's fallback
// rate or a stream's token estimate. Measured usage priced from the table, and
// zero-cost subscription calls, are not flagged.
func TestHTTPDurableRowsFlagEstimatedCosts_Fix16(t *testing.T) {
	for _, tc := range []struct {
		name   string
		model  string
		access AccessType
		stream bool
		want   bool
	}{
		{"unpriced non-stream", "gpt-unpriced-fixture", AccessAPI, false, true},
		{"unpriced stream", "gpt-unpriced-fixture", AccessAPI, true, true},
		{"priced non-stream", "gpt-4o", AccessAPI, false, false},
		{"priced stream", "gpt-4o", AccessAPI, true, true},
		{"subscription stream", "gpt-4o", AccessSubscription, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newBillingUpstream(t, 10, 1, false)
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", tc.model, upstream.URL+"/v1"), Access: tc.access}}, DefaultModel: "openai", CodingModel: "openai"})
			h, store := billingHandler(t, r, false)
			billingChat(t, h, "", "explain what a monad is", tc.stream)
			e := onlyEntry(t, store, tc.stream)
			if e.EstimatedCost != tc.want || (tc.access == AccessAPI) != (e.CostUSD > 0) {
				t.Fatalf("row = %+v, want estimated_cost=%t", e, tc.want)
			}
		})
	}
}
