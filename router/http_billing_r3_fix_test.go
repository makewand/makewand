package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// r3Upstream is a loopback fake of the OpenAI chat-completions, Anthropic
// messages and Gemini generateContent APIs. A stream carries no usage and
// sends unit, perEvent times per SSE event: the request's output cap in units
// times overrun, plus extra, or total units when set. A non-streaming reply
// returns reply and reports usage (1000 prompt and 2000 completion tokens)
// unless noUsage is set. Like the OpenAI API, it refuses max_tokens for a
// reasoning model (limit).
type r3Upstream struct {
	*httptest.Server
	unit     string
	perEvent int
	overrun  int
	extra    int
	total    int
	reply    string
	noUsage  bool

	mu        sync.Mutex
	models    []string
	maxTokens []int
}

func newR3Upstream(t *testing.T, unit string, perEvent, overrun int) *r3Upstream {
	t.Helper()
	u := &r3Upstream{unit: unit, perEvent: perEvent, overrun: overrun, reply: "ok"}
	u.Server = httptest.NewServer(u)
	t.Cleanup(u.Close)
	return u
}

func (u *r3Upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model            string `json:"model"`
		Stream           bool   `json:"stream"`
		GenerationConfig struct {
			MaxOutputTokens int `json:"maxOutputTokens"`
		} `json:"generationConfig"`
		openAIChatLimits
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	api := "openai"
	maxTokens, refused := req.limit(req.Model)
	if strings.HasSuffix(r.URL.Path, "/messages") {
		api = "anthropic"
	} else if _, rest, ok := strings.Cut(r.URL.Path, "/models/"); ok {
		api = "gemini"
		req.Model, _, _ = strings.Cut(rest, ":")
		req.Stream = strings.HasSuffix(rest, ":streamGenerateContent")
		maxTokens = req.GenerationConfig.MaxOutputTokens
	}
	u.mu.Lock()
	u.models = append(u.models, req.Model)
	u.maxTokens = append(u.maxTokens, maxTokens)
	u.mu.Unlock()
	if refused != "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, refused)
		return
	}
	if !req.Stream {
		text, _ := json.Marshal(u.reply)
		switch api {
		case "anthropic":
			usage := `,"usage":{"input_tokens":1000,"output_tokens":2000}`
			if u.noUsage {
				usage = ""
			}
			fmt.Fprintf(w, `{"content":[{"type":"text","text":%s}]%s}`, text, usage)
		case "gemini":
			usage := `,"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":2000}`
			if u.noUsage {
				usage = ""
			}
			fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"text":%s}]},"finishReason":"STOP"}]%s}`, text, usage)
		default:
			usage := `,"usage":{"prompt_tokens":1000,"completion_tokens":2000}`
			if u.noUsage {
				usage = ""
			}
			fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}]%s}`, text, usage)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	total := maxTokens*max(1, u.overrun) + u.extra
	if u.total > 0 {
		total = u.total
	}
	for sent := 0; sent < total && r.Context().Err() == nil; sent += u.perEvent {
		text, _ := json.Marshal(strings.Repeat(u.unit, min(u.perEvent, total-sent)))
		switch api {
		case "anthropic":
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", text)
		case "gemini":
			fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":%s}]}}]}\n\n", text)
		default:
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", text)
		}
	}
	switch api {
	case "anthropic":
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	case "gemini":
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	default:
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func (u *r3Upstream) requests() (models []string, maxTokens []int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...), append([]int(nil), u.maxTokens...)
}

// geminiFailoverRouter mirrors `makewand serve` with a Gemini-family CLI
// installed and a Gemini key: "gemini" is the subscription CLI (failing here)
// and the "gemini-api" sibling calls the configured model.
func geminiFailoverRouter(model, baseURL string) *Router {
	return mustNewRouter(RouterConfig{
		Providers: map[string]ProviderEntry{
			"gemini":     {Provider: &stubProvider{name: "gemini", available: true, failChat: true, failStream: true}, Access: AccessSubscription},
			"gemini-api": {Provider: NewGeminiWithBaseURL("fixture-key", model, baseURL), Access: AccessAPI},
		},
		DefaultModel: "gemini",
		CodingModel:  "gemini",
	})
}

// r3Reservation is the scaled reservation billingHandler makes for a one-message
// request, and the output tokens of the routed model it covers.
func r3Reservation(t *testing.T, r *Router, requestedModel, prompt, routedModel string) (reservation, covered float64) {
	t.Helper()
	msgs := []Message{{Role: "user", Content: prompt}}
	reservation = r.httpReservationUSD(0.0001, &preparedHTTPChat{Router: r, RequestedModel: resolveHTTPModelAlias(requestedModel), Task: ClassifyTask(prompt), Messages: msgs})
	price := tablePrice(t, routedModel)
	input := float64(estimateTokens(buildCLIPrompt(msgs, ""))) * price.Input / 1_000_000
	return reservation, (reservation - input) / (price.Output / 1_000_000)
}

func TestGeminiReportsTheModelItCalls_R3(t *testing.T) {
	g := NewGeminiWithBaseURL("k", "gemini-2.5-flash", "http://127.0.0.1:1")
	if got := reportedModelID(g, ""); got != "gemini-2.5-flash" {
		t.Errorf("gemini reports %q when routing names no model, want its configured gemini-2.5-flash", got)
	}
	if got := reportedModelID(g, "gemini-2.5-pro"); got != "gemini-2.5-pro" {
		t.Errorf("gemini reports %q for requested gemini-2.5-pro", got)
	}
}

// A client pinning "gemini-api" or naming a Gemini model pins the "gemini"
// CLI; when it fails, the "gemini-api" sibling serves the request with its
// configured model. The stream names and is billed at that model, as the
// non-streaming call is - not at the $3/$15 unpriced fallback (50x flash).
func TestHTTPGeminiAPISiblingStreamBilledAtTheModelItCalls_R3(t *testing.T) {
	const flash = "gemini-2.5-flash"
	for _, clientModel := range []string{"gemini-api", "gemini-2.5-pro"} {
		for _, scaled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/scaled=%t", clientModel, scaled), func(t *testing.T) {
				upstream := newR3Upstream(t, "go ", 10, 1)
				h, store := billingHandler(t, geminiFailoverRouter(flash, upstream.URL), scaled)
				_, streamModel := sseDeltas(t, billingChat(t, h, clientModel, "explain what a monad is", true))
				plain := billingChat(t, h, clientModel, "explain what a monad is", false)
				if models, _ := upstream.requests(); len(models) != 2 || models[0] != flash || models[1] != flash {
					t.Fatalf("upstream models = %q, want %q", models, flash)
				}
				if streamModel != flash || !strings.Contains(plain, fmt.Sprintf(`"model":%q`, flash)) {
					t.Fatalf("stream reports model %q (non-stream reply %s), want %q", streamModel, reservationTail(plain, 120), flash)
				}
				price := tablePrice(t, flash)
				for _, stream := range []bool{true, false} {
					e := onlyEntry(t, store, stream)
					if want := billedAt(e, price); e.CostUSD <= 0 || math.Abs(e.CostUSD-want) > 1e-9 {
						t.Fatalf("stream=%t: %d+%d tokens billed $%v, want $%v at %s's price", stream, e.PromptTokens, e.CompletionTokens, e.CostUSD, want, flash)
					}
				}
			})
		}
	}
}

// The reservation ceiling and the stream guard price a "gemini-api" route at
// the model it calls, not at the unpriced fallback.
func TestGeminiAPISiblingCeilingAndGuardPriceTheCalledModel_R3(t *testing.T) {
	r := geminiFailoverRouter("gemini-2.5-flash", "http://127.0.0.1:1")
	// The base provider's tiers (flash, pro) and the configured flash.
	if got, ok := r.httpPriceCeiling(); !ok || got != tablePrice(t, "gemini-2.5-pro") {
		t.Fatalf("ceiling = %+v ok=%v, want gemini-2.5-pro's %+v", got, ok, tablePrice(t, "gemini-2.5-pro"))
	}
	provider, ok := r.getProvider("gemini-api")
	if !ok {
		t.Fatal("no gemini-api provider")
	}
	msgs := []Message{{Role: "user", Content: "explain what a monad is"}}
	const limit = 0.01
	guard := newHTTPStreamSpendGuard(r, limit, RouteResult{Actual: "gemini-api", Provider: provider}, msgs, "", Usage{})
	flash := tablePrice(t, "gemini-2.5-flash")
	input := float64(estimateTokens(buildCLIPrompt(msgs, ""))) * flash.Input / 1_000_000
	if want := math.Floor((limit - input) / (flash.Output / 1_000_000)); guard == nil || math.Abs(guard.coveredTokens-want) > 1 {
		t.Fatalf("guard = %+v, want %v output tokens covered at gemini-2.5-flash's price", guard, want)
	}
}

func TestStreamTokenFloorBoundsWhitespaceFreeText_R3(t *testing.T) {
	floor := func(chunks ...string) int {
		var f streamTokenFloor
		for _, chunk := range chunks {
			f.add(chunk)
		}
		return f.tokens()
	}
	for _, tc := range []struct {
		name   string
		chunks []string
		want   int
	}{
		// Past the first token, at least one per emoji, 8 ASCII characters
		// or 4 dense-script characters.
		{"emoji run", []string{strings.Repeat("😀", 1000)}, 999},
		{"emoji run in pieces", r3Split(strings.Repeat("😀", 1000), 7), 999},
		{"letter run", r3Split(strings.Repeat("x", 8000), 13), 999},
		{"dense run", r3Split(strings.Repeat("中", 4000), 9), 999},
		// GLM-4.5 merges 16 NULs a token (see runMergeLimits).
		{"NUL run", []string{strings.Repeat("\x00", 8000)}, 500},
		{"flags: two regional indicators a token", []string{strings.Repeat("🇺🇸", 1000)}, 999},
		{"symbols beyond ASCII: 8 bytes a token", []string{strings.Repeat("→", 8000)}, 2999},
		// Long single tokens a reply of max_tokens may consist of.
		{"16-letter word", []string{"responsibilities"}, 1},
		{"17-letter word", []string{"abcdefghijklmnopq"}, 2},
		{"64-dash separator lines", r3Split(strings.Repeat(strings.Repeat("-", 64)+"\n", 4096), 650), 4096},
		{"80-column separator", []string{"#" + strings.Repeat("=", 79)}, 1},
		{"long separator: 64 characters a token", []string{strings.Repeat("-", 64*10+1)}, 10},
		{"separator characters alternating", []string{strings.Repeat("-=", 400)}, 99},
		{"emoji word", []string{"😀"}, 1},
		{"one-token words", r3Split(strings.Repeat("go ", 100), 7), 100},
	} {
		if got := floor(tc.chunks...); got != tc.want {
			t.Errorf("%s: floor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// However a text is split into chunks, its floor is the same, and appending
// text never lowers it (a cut stream stays cut).
func TestStreamTokenFloorIsSplitInvariantAndMonotonic_R3(t *testing.T) {
	units := []string{"go ", "word", " ", "\n", "😀", "🇺🇸", "x", "中", "文", "-", "=", "#", "--", ".", "→", "é", "\x00", "a-b", "\u3000"}
	rng := rand.New(rand.NewSource(3)) //nolint:gosec // G404: a seeded generator keeps the generated test inputs reproducible.
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for n := rng.Intn(200); n > 0; n-- {
			b.WriteString(units[rng.Intn(len(units))])
		}
		text := b.String()
		var whole streamTokenFloor
		whole.add(text)
		var pieces streamTokenFloor
		last := 0
		for _, chunk := range r3Split(text, 1+rng.Intn(9)) {
			pieces.add(chunk)
			if got := pieces.tokens(); got < last {
				t.Fatalf("floor fell from %d to %d appending %q to %q", last, got, chunk, text)
			}
			last = pieces.tokens()
		}
		if pieces.tokens() != whole.tokens() {
			t.Fatalf("floor of %q = %d whole but %d in chunks", text, whole.tokens(), pieces.tokens())
		}
	}
}

// r3Split cuts text into chunks of size runes: decoded SSE content always ends
// on a rune boundary.
func r3Split(text string, size int) []string {
	var chunks []string
	runes := []rune(text)
	for len(runes) > size {
		chunks = append(chunks, string(runes[:size]))
		runes = runes[size:]
	}
	return append(chunks, string(runes))
}

// A Claude API route (sonnet, reserved at the opus ceiling) whose provider
// ignores its output cap is stopped right past the tokens the reservation
// covers - also when it streams emoji or letters without whitespace, which
// were cut only at 16 characters a token, after 16 times the reservation in
// real emoji tokens - and settles at the reservation, not at the word
// heuristic's 1.3x (words) to 4.6x (emoji, letters).
func TestHTTPStreamGuardStopsWhitespaceFreeRunawaysNearTheReservation_R3(t *testing.T) {
	const prompt = "explain what a monad is"
	for _, tc := range []struct {
		name, unit string
		// tokens a real tokenizer makes of one unit, at least
		unitTokens float64
	}{
		{"emoji", "😀", 1},
		{"letters", "x", 1.0 / 16},
		{"words", "go ", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const perEvent = 40
			upstream := newR3Upstream(t, tc.unit, perEvent, 100)
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: NewClaudeWithBaseURL("k", "", upstream.URL), Access: AccessAPI}}, DefaultModel: "claude", CodingModel: "claude"})
			h, store := billingHandler(t, r, true)
			body := billingChat(t, h, "", prompt, true)
			content, _ := sseDeltas(t, body)
			models, maxTokens := upstream.requests()
			if len(models) != 1 {
				t.Fatalf("upstream models = %q", models)
			}
			reservation, covered := r3Reservation(t, r, "", prompt, models[0])
			units := strings.Count(content, tc.unit)
			if !strings.Contains(body, "budget_reservation_exceeded") || units >= 100*maxTokens[0] {
				t.Fatalf("runaway of %d units (max_tokens=%v) was not stopped", units, maxTokens)
			}
			var floor streamTokenFloor
			floor.add(content)
			if got := float64(floor.tokens()); got <= covered || got > covered+perEvent+1 {
				t.Fatalf("stopped at %v floor tokens (%d units); want just past the %v the reservation covers", got, units, covered)
			}
			if real := float64(units) * tc.unitTokens; real > 1.01*covered+perEvent {
				t.Fatalf("stopped after %d units, at least %v real tokens: %.1fx the %v covered", units, real, real/covered, covered)
			}
			e := onlyEntry(t, store, true)
			if !e.EstimatedCost || e.CostUSD < reservation-1e-6 || e.CostUSD > 1.01*reservation {
				t.Fatalf("stopped runaway settled $%v (estimated=%t), want about the $%v reservation (%.2fx)", e.CostUSD, e.EstimatedCost, reservation, e.CostUSD/reservation)
			}
		})
	}
}

// A reply of exactly max_tokens is never cut, also when it consists of long
// single tokens such as separator lines, on a route whose model is the ceiling
// (the reservation covers exactly max_tokens); one more line is. The row
// settles at the cost of max_tokens, not the 18x the word heuristic makes of
// a 64-character line.
func TestHTTPStreamGuardNeverCutsSeparatorLinesWithinMaxTokens_R3(t *testing.T) {
	const prompt = "explain what a monad is"
	for _, unit := range []string{strings.Repeat("-", 64) + "\n", "#" + strings.Repeat("=", 79) + "\n", strings.Repeat("_", 32) + " ", "go "} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%q/extra=%d", unit[:min(4, len(unit))], extra), func(t *testing.T) {
				upstream := newR3Upstream(t, unit, 10, 1)
				upstream.extra = extra
				r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-4o", upstream.URL+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
				h, store := billingHandler(t, r, true)
				body := billingChat(t, h, "", prompt, true)
				content, _ := sseDeltas(t, body)
				_, maxTokens := upstream.requests()
				units := strings.Count(content, unit)
				cut := strings.Contains(body, "budget_reservation_exceeded")
				if extra == 0 && (cut || units != maxTokens[0]) {
					t.Fatalf("reply of max_tokens=%d units delivered %d (cut=%t)", maxTokens[0], units, cut)
				}
				if extra == 1 {
					if !cut {
						t.Fatalf("unit %d past max_tokens=%d was not cut (delivered %d)", maxTokens[0]+1, maxTokens[0], units)
					}
					return
				}
				reservation, _ := r3Reservation(t, r, "", prompt, "gpt-4o")
				e := onlyEntry(t, store, true)
				if e.CompletionTokens != maxTokens[0] || e.CostUSD > reservation+1e-9 || math.Abs(e.CostUSD-billedAt(e, tablePrice(t, "gpt-4o"))) > 1e-9 {
					t.Fatalf("row = %+v; want %d completion tokens, at most the $%v reservation", e, maxTokens[0], reservation)
				}
			})
		}
	}
}

// A reply of exactly max_tokens whose usage is estimated settles at the cost
// of max_tokens: the word heuristic made a 4096-word reply 5324 tokens and
// settled o1 at 1.3x its reservation.
func TestHTTPEstimatedReplyOfMaxTokensSettlesAtItsCost_R3(t *testing.T) {
	const prompt = "explain what a monad is"
	for _, tc := range []struct {
		name   string
		router func(baseURL string) *Router
		model  string // the client's
	}{
		{"openai o1", func(u string) *Router {
			return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "o1", u+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
		}, ""},
		{"claude-api sibling", func(u string) *Router { return cliFailoverRouter("claude-opus-4-20250514", u) }, "claude-sonnet-4"},
	} {
		for _, scaled := range []bool{false, true} {
			for _, perEvent := range []int{1, 10} {
				t.Run(fmt.Sprintf("%s/scaled=%t/%d", tc.name, scaled, perEvent), func(t *testing.T) {
					upstream := newR3Upstream(t, "go ", perEvent, 1)
					r := tc.router(upstream.URL)
					h, store := billingHandler(t, r, scaled)
					body := billingChat(t, h, tc.model, prompt, true)
					models, maxTokens := upstream.requests()
					if strings.Contains(body, "budget_reservation_exceeded") || len(models) != 1 {
						t.Fatalf("reply cut or not served: models=%q tail=%q", models, reservationTail(body, 200))
					}
					price := tablePrice(t, models[0])
					e := onlyEntry(t, store, true)
					msgs := []Message{{Role: "user", Content: prompt}}
					maxCost := (float64(estimateTokens(buildCLIPrompt(msgs, "")))*price.Input + float64(maxTokens[0])*price.Output) / 1_000_000
					if e.CompletionTokens != maxTokens[0] || math.Abs(e.CostUSD-maxCost) > 1e-9 || !e.EstimatedCost {
						t.Fatalf("row = %+v; want %d completion tokens at the $%v cost of max_tokens", e, maxTokens[0], maxCost)
					}
					if reservation, _ := r3Reservation(t, r, tc.model, prompt, models[0]); scaled && e.CostUSD > reservation+1e-9 {
						t.Fatalf("settled $%v above the $%v reservation", e.CostUSD, reservation)
					}
				})
			}
		}
	}
}

func TestEstimateOutputTokens_R3(t *testing.T) {
	words := func(n int) string { return strings.Repeat("go ", n) }
	sep := strings.Repeat(strings.Repeat("-", 64)+"\n", 100)
	for _, tc := range []struct {
		name      string
		output    string
		maxTokens int
		want      int
	}{
		{"within the cap: the heuristic", words(100), 4096, 130},
		{"cap unknown: the heuristic", words(4096), 0, 5324},
		{"exactly max_tokens words: the cap", words(4096), 4096, 4096},
		{"heuristic above the cap, text within it: the cap", sep, 100, 100},
		{"text past the cap: its floor", words(5000), 4096, 5000},
		{"text past the cap: the heuristic when lower", strings.Repeat("😀", 5000), 1000, estimateTokens(strings.Repeat("😀", 5000))},
		{"separator lines past the cap: the floor, not 18x", sep, 50, 100},
	} {
		if got := estimateOutputTokens(tc.output, tc.maxTokens); got != tc.want {
			t.Errorf("%s: estimateOutputTokens = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A non-streaming reply without a usage block is estimated and flagged as
// estimated, as a stream is, for every API provider - not billed $0 as a
// measured cost.
func TestHTTPNonStreamReplyWithoutUsageIsEstimated_R3(t *testing.T) {
	const prompt = "explain what a monad is"
	reply := strings.Repeat("abcd ", 50)
	for _, tc := range []struct {
		name   string
		router func(baseURL string) *Router
	}{
		{"openai o1", func(u string) *Router {
			return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "o1", u+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
		}},
		{"openai gpt-4o-mini", func(u string) *Router {
			return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-4o-mini", u+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
		}},
		{"claude", func(u string) *Router {
			return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: NewClaudeWithBaseURL("k", "", u), Access: AccessAPI}}, DefaultModel: "claude", CodingModel: "claude"})
		}},
		{"gemini", func(u string) *Router {
			return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"gemini": {Provider: NewGeminiWithBaseURL("k", "", u), Access: AccessAPI}}, DefaultModel: "gemini", CodingModel: "gemini"})
		}},
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			t.Run(tc.name+path, func(t *testing.T) {
				upstream := newR3Upstream(t, "go ", 10, 1)
				upstream.reply, upstream.noUsage = reply, true
				h, store := billingHandler(t, tc.router(upstream.URL), false)
				rec := r3Post(t, h, path, "", prompt, false)
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				models, _ := upstream.requests()
				e := onlyEntry(t, store, false)
				wantIn, wantOut := estimateTokens(buildCLIPrompt([]Message{{Role: "user", Content: prompt}}, "")), estimateTokens(reply)
				if !e.EstimatedCost || e.PromptTokens != wantIn || e.CompletionTokens != wantOut {
					t.Fatalf("row = %+v; want an estimated %d+%d tokens", e, wantIn, wantOut)
				}
				if want := billedAt(e, tablePrice(t, models[0])); e.CostUSD <= 0 || math.Abs(e.CostUSD-want) > 1e-9 {
					t.Fatalf("billed $%v, want $%v at %s's price", e.CostUSD, want, models[0])
				}
				if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"completion_tokens":%d`, wantOut)) {
					t.Fatalf("reply usage does not match the row: %s", reservationTail(rec.Body.String(), 200))
				}
			})
		}
	}
}

// Any API provider that reports no usage for a non-streaming reply is
// estimated at the facade, as its streams are (here an unpriced custom one, at
// the unpriced fallback rate).
func TestHTTPNonStreamCustomProviderWithoutUsageIsEstimated_R3(t *testing.T) {
	const prompt = "explain what a monad is"
	reply := strings.Repeat("word ", 300)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"custom": {Provider: &stubProvider{name: "custom", available: true, response: reply}, Access: AccessAPI}}, DefaultModel: "custom", CodingModel: "custom"})
			h, store := billingHandler(t, r, false)
			if rec := r3Post(t, h, path, "custom", prompt, false); rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			e := onlyEntry(t, store, false)
			if want := billedAt(e, unpricedFallbackCost()); !e.EstimatedCost || e.CompletionTokens != estimateTokens(reply) || e.PromptTokens == 0 || math.Abs(e.CostUSD-want) > 1e-9 {
				t.Fatalf("row = %+v; want an estimated cost $%v for %d completion tokens", e, want, estimateTokens(reply))
			}
		})
	}
}

// The API providers report a reply without usage as an unmeasured estimate
// (capped at max_tokens) instead of a measured $0, so execution accounting and
// Power-mode ensembles see the spend; a reply with usage stays measured.
func TestAPIProvidersEstimateRepliesWithoutUsage_R3(t *testing.T) {
	providers := func(u string) []Provider {
		return []Provider{NewOpenAIWithBaseURL("k", "gpt-4o", u+"/v1"), NewClaudeWithBaseURL("k", "claude-sonnet-4-20250514", u), NewGeminiWithBaseURL("k", "gemini-2.5-pro", u)}
	}
	msgs := []Message{{Role: "user", Content: "explain what a monad is"}}
	for _, noUsage := range []bool{true, false} {
		upstream := newR3Upstream(t, "go ", 10, 1)
		upstream.reply, upstream.noUsage = strings.Repeat("go ", 500), noUsage
		for _, p := range providers(upstream.URL) {
			_, usage, err := p.Chat(context.Background(), msgs, "sys", 200)
			if err != nil {
				t.Fatalf("%s: %v", p.Name(), err)
			}
			model := reportedModelID(p, "")
			if !noUsage {
				if !usage.MeasuredTokens || !usage.MeasuredCost || usage.InputTokens != 1000 || usage.OutputTokens != 2000 {
					t.Errorf("%s: reported usage = %+v, want measured 1000+2000", p.Name(), usage)
				}
				continue
			}
			// 500 one-token words past a 200-token cap: billed at the floor, 500.
			wantIn := estimateTokens(buildCLIPrompt(msgs, "sys"))
			want := EstimateCost(model, wantIn, 500)
			if usage.MeasuredTokens || usage.MeasuredCost || usage.InputTokens != wantIn || usage.OutputTokens != 500 || math.Abs(usage.Cost-want) > 1e-12 || usage.Cost <= 0 {
				t.Errorf("%s: usage without a usage block = %+v, want an unmeasured %d+500 tokens at $%v", p.Name(), usage, wantIn, want)
			}
		}
	}
}

func r3Post(t *testing.T, h http.Handler, path, model, prompt string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":%q}]}`, model, stream, prompt)
	if path == "/v1/responses" {
		body = fmt.Sprintf(`{"model":%q,"stream":%t,"input":%q}`, model, stream, prompt)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
