package router

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// r4MeasuredReplies are replies the stream guard must deliver whole when they
// fit in max_tokens: each is unit repeated count times (bytes long), with the
// fewest tokens any calibration vocabulary encodes it to, which vocabulary
// that is, and GPT-4o's count. Measured with tiktoken 0.13 (o200k_base,
// cl100k_base) and tokenizers 0.22 (Llama 3.1, Qwen 2.5 and 3, DeepSeek-V3,
// Gemma 2 and 3, Mistral Nemo, GLM-4.5, Grok-1).
var r4MeasuredReplies = []struct {
	name         string
	unit         string
	count, bytes int
	least        int
	leastVocab   string
	o200k        int
}{
	{"box-drawing lines", strings.Repeat("─", 64) + "\n", 200, 38600, 1000, "o200k_base", 1000},
	{"box table border", "┌──────┬──────┐\n", 600, 27600, 3600, "gemma3", 6600},
	{"box table row", "│ name     │ value    │\n", 300, 9000, 2400, "o200k_base", 2400},
	{"box table rule", "├" + strings.Repeat("─", 10) + "┼──────────┤\n", 300, 21000, 2400, "qwen25", 3000},
	{"heavy lines", strings.Repeat("━", 64) + "\n", 200, 38600, 1000, "llama31", 1800},
	{"double lines", strings.Repeat("═", 64) + "\n", 200, 38600, 1800, "o200k_base", 1800},
	{"double table", "╔" + strings.Repeat("═", 10) + "╦══════════╗\n", 300, 21000, 2400, "gemma2", 3000},
	{"em dash lines", strings.Repeat("—", 40) + "\n", 300, 36300, 1200, "o200k_base", 1200},
	{"ellipsis lines", strings.Repeat("…", 16) + "\n", 300, 14700, 600, "o200k_base", 600},
	{"block bars", strings.Repeat("█", 32) + "\n", 300, 29100, 1501, "grok1", 2700},
	{"middle dot leaders", "Chapter " + strings.Repeat("·", 40) + " 7\n", 300, 27300, 3300, "deepseekv3", 7500},
	{"dash lines", strings.Repeat("-", 64) + "\n", 2000, 130000, 4000, "o200k_base", 4000},
	{"equals ruler", "#" + strings.Repeat("=", 79) + "\n", 2000, 162000, 4000, "cl100k_base", 6000},
	{"caret lines", strings.Repeat("^", 64) + "\n", 300, 19500, 1503, "grok1", 2700},
	{"tilde lines", strings.Repeat("~", 64) + "\n", 300, 19500, 900, "o200k_base", 900},
	{"plus lines", strings.Repeat("+", 64) + "\n", 300, 19500, 900, "o200k_base", 900},
	{"underscore lines", strings.Repeat("_", 64) + "\n", 300, 19500, 600, "o200k_base", 600},
	{"dot leaders", strings.Repeat(".", 64) + "\n", 300, 19500, 600, "o200k_base", 600},
	{"long dash lines", strings.Repeat("-", 256) + "\n", 300, 77100, 1500, "o200k_base", 1500},
	{"star ratings", "★★★★★ ", 1000, 16000, 1001, "gemma2", 2000},
	{"markdown table", "| a | b |\n|---|---|\n", 500, 10000, 3500, "deepseekv3", 5000},
	{"indented code", strings.Repeat(" ", 8) + "return x\n", 500, 8500, 2000, "o200k_base", 2000},
	{"wide space gaps", "x" + strings.Repeat(" ", 200), 300, 60300, 900, "o200k_base", 900},
	{"blank line runs", "x" + strings.Repeat("\n", 40), 300, 12300, 900, "cl100k_base", 1200},
	{"go code", "func main() {\n\tfmt.Println(\"hello\")\n\tfor i := 0; i < 10; i++ {\n\t\tif i%2 == 0 {\n\t\t\tcontinue\n\t\t}\n\t}\n}\n", 100, 10000, 4100, "o200k_base", 4100},
	{"python code", "def f(x):\n    return [y ** 2 for y in range(x) if y % 3 == 0]  # comment\n\nclass A(B):\n    pass\n", 100, 9500, 3700, "o200k_base", 3700},
	{"english", "Hello, my name is GPT-4o. I'm a new type of language model, it's nice to meet you!\n", 100, 8300, 2400, "o200k_base", 2400},
	{"russian", "Привет, меня зовут GPT-4o. Я — новая языковая модель, приятно познакомиться!\n", 100, 13400, 2100, "gemma2", 2300},
	{"hindi", "नमस्ते, मेरा नाम जीपीटी-4o है। मैं एक नए प्रकार का भाषा मॉडल हूँ। आपसे मिलकर अच्छा लगा!\n", 100, 22000, 2700, "gemma3", 3100},
	{"chinese", "你好，我的名字是GPT-4o。我是一种新型的语言模型，很高兴见到你!\n", 100, 8900, 2000, "deepseekv3", 2400},
	{"japanese", "こんにちわ、私の名前はGPT−4oです。私は新しいタイプの言語モデルです、初めまして\n", 100, 11700, 2300, "gemma2", 2800},
	{"emoji", "😀", 4096, 16384, 4096, "o200k_base", 4096},
	{"flags", "🇺🇸", 1000, 8000, 1000, "gemma2", 4000},
	{"family", "👨\u200d👩\u200d👧\u200d👦 ", 300, 7800, 2400, "gemma2", 3301},
	{"hex", "0123456789abcdef", 300, 4800, 1500, "o200k_base", 1500},
	{"one-token words", "go ", 4096, 12288, 4097, "o200k_base", 4097},
}

// The floor of a reply never exceeds the tokens any calibration vocabulary
// encodes it to, so a reply that fits in max_tokens is never cut. Runs of one
// non-ASCII character counted 2.7 characters a token: 200 lines of 64
// box-drawing characters 4,600 tokens (GPT-4o: 1,000), 300 lines of 40 em
// dashes 4,200 (GPT-4o: 1,200).
func TestStreamTokenFloorStaysBelowMeasuredTokenCounts_R4(t *testing.T) {
	for _, tc := range r4MeasuredReplies {
		text := strings.Repeat(tc.unit, tc.count)
		if len(text) != tc.bytes {
			t.Fatalf("%s: text is %d bytes, measured text was %d", tc.name, len(text), tc.bytes)
		}
		var f streamTokenFloor
		f.add(text)
		if got := f.tokens(); got > tc.least {
			t.Errorf("%s: floor = %d, above the %d tokens %s encodes it to (o200k_base: %d)", tc.name, got, tc.least, tc.leastVocab, tc.o200k)
		}
	}
}

func TestStreamTokenFloorCountsRunsOfAnyRepeatedCharacter_R4(t *testing.T) {
	floor := func(text string) int {
		var f streamTokenFloor
		f.add(text)
		return f.tokens()
	}
	box := strings.Repeat("─", 64)
	for _, tc := range []struct {
		name string
		text string
		want int
	}{
		// Past its first character, a repeat counts 1/16 of a token: GPT-4o
		// encodes a 64-character box-drawing line in four.
		{"box-drawing line", box, 4},
		{"box-drawing lines", strings.Repeat(box+"\n", 200), 800},
		{"short box runs in a table border", "┌──────┬──────┐", 2},
		{"em dash rule", strings.Repeat("—", 40), 2},
		{"five stars", "★★★★★", 1},
		// Floods of characters merged into fewer characters per token than a
		// dash are cut correspondingly earlier (64 characters a token before).
		{"caret flood", strings.Repeat("^", 65536), 4096},
		{"plus flood", strings.Repeat("+", 65536), 2048},
		{"tilde flood", strings.Repeat("~", 65536), 2048},
		{"dash flood", strings.Repeat("-", 65536), 1024},
		{"dot flood: DeepSeek-V3 packs 128 dots a token", strings.Repeat(".", 65536), 512},
		// Runs of one whitespace character count too (before: never).
		{"newline flood", strings.Repeat("\n", 65536), 2048},
		{"space flood", strings.Repeat(" ", 65536), 128},
		{"tab run", strings.Repeat("\t", 64), 2},
		{"indentation", "x" + strings.Repeat(" ", 40) + "y", 2},
		// Alternating whitespace is no run of one character: still nothing.
		{"mixed whitespace", strings.Repeat(" \n", 1000), 0},
		// Characters no vocabulary merges keep their weight.
		{"arrows", strings.Repeat("→", 8000), 2999},
		{"box corners", strings.Repeat("┌", 8000), 2999},
		// Letters, digits and emoji keep their weight, so their runaways stay
		// bounded near the reservation.
		{"emoji run", strings.Repeat("😀", 1000), 999},
		{"letter run", strings.Repeat("x", 8000), 999},
		{"dense run", strings.Repeat("中", 4000), 999},
	} {
		if got := floor(tc.text); got != tc.want {
			t.Errorf("%s: floor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A provider that forwards raw bytes can split a UTF-8 sequence across stream
// chunks; the floor counts the character once, as in one piece (100 emoji
// sent three bytes a chunk counted 149 tokens instead of 99).
func TestStreamTokenFloorCarriesUTF8SequencesAcrossChunks_R4(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("😀", 100),
		strings.Repeat(strings.Repeat("─", 64)+"\n", 20),
		"Grüße — 中文 Привет 🇺🇸👨\u200d👩\u200d👧 नमस्ते ★★★★★ ┌──┬──┐",
	} {
		var whole streamTokenFloor
		whole.add(text)
		for size := 1; size <= 5; size++ {
			var pieces streamTokenFloor
			last := 0
			for i := 0; i < len(text); i += size {
				pieces.add(text[i:min(i+size, len(text))])
				if got := pieces.tokens(); got < last {
					t.Fatalf("floor fell from %d to %d at byte %d of %q", last, got, i, text)
				}
				last = pieces.tokens()
			}
			if pieces.tokens() != whole.tokens() {
				t.Errorf("%d-byte chunks of %q: floor = %d, want %d as in one piece", size, text, pieces.tokens(), whole.tokens())
			}
		}
	}
	var f streamTokenFloor
	f.add("😀"[:2])
	if got := f.tokens(); got != 0 {
		t.Fatalf("half an emoji counts %d tokens before the rest arrives", got)
	}
	f.add("😀"[2:])
	if got := f.tokens(); got != 1 {
		t.Fatalf("an emoji completed by the next chunk counts %d tokens, want 1", got)
	}
}

// However a text is cut into chunks - even inside a UTF-8 sequence - its floor
// is the same, and appending text never lowers it (a cut stream stays cut).
func TestStreamTokenFloorIsByteSplitInvariantAndMonotonic_R4(t *testing.T) {
	units := []string{"go ", "word", " ", "  ", "\n", "\n\n", "\t", "😀", "🇺🇸", "x", "中", "─", "━", "═", "┌", "│", "-", "=", "^", "+", ".", "—", "…", "★", "·", "é", "\u3000", "\x00", "\xff"}
	rng := rand.New(rand.NewSource(4)) //nolint:gosec // G404: a seeded generator keeps the generated test inputs reproducible.
	for i := 0; i < 500; i++ {
		var b strings.Builder
		for n := rng.Intn(300); n > 0; n-- {
			b.WriteString(strings.Repeat(units[rng.Intn(len(units))], 1+rng.Intn(3)*rng.Intn(40)))
		}
		text := b.String()
		var whole streamTokenFloor
		whole.add(text)
		var pieces streamTokenFloor
		last := 0
		for at := 0; at < len(text); {
			next := min(len(text), at+1+rng.Intn(12))
			pieces.add(text[at:next])
			if got := pieces.tokens(); got < last {
				t.Fatalf("floor fell from %d to %d appending %q", last, got, text[at:next])
			}
			last = pieces.tokens()
			at = next
		}
		if pieces.tokens() != whole.tokens() {
			t.Fatalf("floor of %q = %d whole but %d in byte chunks", text, whole.tokens(), pieces.tokens())
		}
	}
}

// Box-drawing replies that fit in max_tokens are delivered whole on routes
// whose reservation covers exactly max_tokens (the route's model is the price
// ceiling); 200 and 455 lines of 64 box-drawing characters were cut at 180.
func TestHTTPStreamGuardDeliversBoxDrawingRepliesWithinMaxTokens_R4(t *testing.T) {
	const prompt = "explain what a monad is"
	line := strings.Repeat("─", 64) + "\n"
	openai := func(u string) *Router {
		return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-4o", u+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
	}
	claude := func(u string) *Router {
		return mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"claude": {Provider: NewClaudeWithBaseURL("k", "", u), Access: AccessAPI}}, DefaultModel: "claude", CodingModel: "claude"})
	}
	for _, tc := range []struct {
		name        string
		router      func(baseURL string) *Router
		model, mode string
		lines       int
		calls       string // model the upstream must receive: the ceiling, so the reservation covers exactly max_tokens
	}{
		{"gpt-4o, 200 lines", openai, "", "", 200, "gpt-4o"},
		{"gpt-4o, 455 lines", openai, "", "", 455, "gpt-4o"},
		{"claude pinned in power mode, 200 lines", claude, "claude", "power", 200, "claude-opus-4-20250514"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newR3Upstream(t, line, 5, 1)
			upstream.total = tc.lines
			h, store := billingHandler(t, tc.router(upstream.URL), true)
			payload, _ := json.Marshal(map[string]any{"model": tc.model, "mode": tc.mode, "stream": true, "messages": []map[string]string{{"role": "user", "content": prompt}}})
			rec := postFix16Chat(t, h, "secret", string(payload))
			body := rec.Body.String()
			content, _ := sseDeltas(t, body)
			models, maxTokens := upstream.requests()
			if len(models) != 1 || models[0] != tc.calls {
				t.Fatalf("upstream models = %q, want %s", models, tc.calls)
			}
			if lines := strings.Count(content, line); strings.Contains(body, "budget_reservation_exceeded") || lines != tc.lines {
				t.Fatalf("a %d-line box-drawing reply (max_tokens=%v) delivered %d lines; tail=%q", tc.lines, maxTokens, lines, reservationTail(body, 200))
			}
			if logged := store.loggedEntry(t, true); logged.UncertainCost {
				t.Fatalf("complete reply logged with an uncertain cost: %+v", logged)
			}
		})
	}
}

// A provider that ignores its output cap and floods one repeated character
// is stopped once the text passes what the reservation covers at the densest
// packing of that character: 16 carets, 32 plus signs or tildes, 64 dashes, 16
// box-drawing characters or 32 newlines a token. Carets, plus signs and tildes
// ran to 64 characters a token, box-drawing characters were cut at 2.7, and a
// newline flood was never cut.
func TestHTTPStreamGuardStopsRepeatedCharacterFloodsAtTheirMergeLimit_R4(t *testing.T) {
	const prompt = "explain what a monad is"
	for _, tc := range []struct {
		name, unit string
		limit      int // characters per token
	}{
		{"carets", "^", 16},
		{"plus signs", "+", 32},
		{"tildes", "~", 32},
		{"dashes", "-", 64},
		{"box drawing", "─", 16},
		{"newlines", "\n", 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const perEvent = 400
			upstream := newR3Upstream(t, tc.unit, perEvent, 100)
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"openai": {Provider: NewOpenAIWithBaseURL("k", "gpt-4o", upstream.URL+"/v1"), Access: AccessAPI}}, DefaultModel: "openai", CodingModel: "openai"})
			h, _ := billingHandler(t, r, true)
			payload, _ := json.Marshal(map[string]any{"stream": true, "messages": []map[string]string{{"role": "user", "content": prompt}}})
			body := postFix16Chat(t, h, "secret", string(payload)).Body.String()
			content, _ := sseDeltas(t, body)
			_, maxTokens := upstream.requests()
			units := strings.Count(content, tc.unit)
			if !strings.Contains(body, "budget_reservation_exceeded") {
				t.Fatalf("a flood of %d %q (max_tokens=%v) was not stopped", units, tc.unit, maxTokens)
			}
			_, covered := r3Reservation(t, r, "", prompt, "gpt-4o")
			if lo, hi := (covered-1)*float64(tc.limit), (covered+2)*float64(tc.limit)+perEvent; float64(units) < lo || float64(units) > hi {
				t.Fatalf("flood stopped after %d %q; want about %.0f, %d a token for the %.0f tokens covered", units, tc.unit, covered*float64(tc.limit), tc.limit, covered)
			}
		})
	}
}

// splitUTF8Provider is a custom streaming provider that forwards its reply in
// chunks of size bytes, splitting UTF-8 sequences: max_tokens repeats of unit,
// plus extra.
type splitUTF8Provider struct {
	unit        string
	size, extra int
}

func (p *splitUTF8Provider) Name() string      { return "custom" }
func (p *splitUTF8Provider) IsAvailable() bool { return true }
func (p *splitUTF8Provider) Chat(context.Context, []Message, string, int) (string, Usage, error) {
	return "ok", Usage{}, nil
}

func (p *splitUTF8Provider) ChatStream(ctx context.Context, _ []Message, _ string, maxTokens int) (<-chan StreamChunk, error) {
	text := strings.Repeat(p.unit, maxTokens+p.extra)
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		for i := 0; i < len(text); i += p.size {
			select {
			case ch <- StreamChunk{Content: text[i:min(i+p.size, len(text))]}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case ch <- StreamChunk{Done: true}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// A custom provider streaming max_tokens emoji three bytes a chunk is not
// cut (it was, at two thirds, each split emoji counting as one and a half
// tokens); two emoji more are.
func TestHTTPStreamGuardCountsUTF8SplitAcrossChunksOnce_R4(t *testing.T) {
	for _, extra := range []int{0, 2} {
		t.Run(fmt.Sprintf("extra=%d", extra), func(t *testing.T) {
			r := mustNewRouter(RouterConfig{Providers: map[string]ProviderEntry{"custom": {Provider: &splitUTF8Provider{unit: "😀", size: 3, extra: extra}, Access: AccessAPI}}, DefaultModel: "custom", CodingModel: "custom"})
			h, _ := billingHandler(t, r, true)
			payload, _ := json.Marshal(map[string]any{"stream": true, "messages": []map[string]string{{"role": "user", "content": "explain what a monad is"}}})
			body := postFix16Chat(t, h, "secret", string(payload)).Body.String()
			if cut := strings.Contains(body, "budget_reservation_exceeded"); cut != (extra > 0) {
				t.Fatalf("max_tokens%+d emoji in 3-byte chunks: cut=%t; tail=%q", extra, cut, reservationTail(body, 200))
			}
		})
	}
}
