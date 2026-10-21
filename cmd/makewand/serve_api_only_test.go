package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
	"github.com/makewand/makewand/internal/testfixture"
	"github.com/makewand/makewand/router"
	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

// apiOnlyUpstream is a loopback fake of the OpenAI chat-completions API. Plain
// replies report 1000 prompt and 2000 completion tokens; streams report none.
// As api.openai.com does, it answers a request that sends an o-series or GPT-5
// model max_tokens with 400 unsupported_parameter: those models take only
// max_completion_tokens.
type apiOnlyUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	models  []string
	rejects map[string]int // model -> HTTP status it is answered with
}

func (u *apiOnlyUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string          `json:"model"`
		Stream    bool            `json:"stream"`
		MaxTokens json.RawMessage `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.models = append(u.models, req.Model)
	status := u.rejects[req.Model]
	u.mu.Unlock()
	model := strings.ToLower(req.Model)
	reasoning := strings.HasPrefix(model, "gpt-5") || len(model) > 1 && model[0] == 'o' && model[1] >= '0' && model[1] <= '9'
	if reasoning && req.MaxTokens != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`)
		return
	}
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":"rejected %s","type":"invalid_request_error"}}`, req.Model)
		return
	}
	if !req.Stream {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"plain answer"}}],"usage":{"prompt_tokens":1000,"completion_tokens":2000}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"streamed answer\"}}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (u *apiOnlyUpstream) take() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	models := u.models
	u.models = nil
	return models
}

// reject makes the upstream answer every request for model with status.
func (u *apiOnlyUpstream) reject(model string, status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.rejects == nil {
		u.rejects = map[string]int{}
	}
	u.rejects[model] = status
}

type apiOnlyUsage struct {
	mu      sync.Mutex
	entries []serverusage.Entry
}

func (l *apiOnlyUsage) Log(entry serverusage.Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	return nil
}

func (l *apiOnlyUsage) take() []serverusage.Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.entries
	l.entries = nil
	return entries
}

// apiOnlyServe builds the router and HTTP facade `makewand serve` runs with
// only OPENAI_API_KEY under MAKEWAND_API_POLICY=allow_paid: the documented
// Docker shape, which has no subscription CLI.
func apiOnlyServe(t *testing.T, openAIModel string) (http.Handler, *apiOnlyUpstream, *apiOnlyUsage) {
	t.Helper()
	return apiOnlyServeWith(t, apiOnlyDeployment{openAIModel: openAIModel})
}

// apiOnlyDeployment varies the deployment apiOnlyServeWith builds.
type apiOnlyDeployment struct {
	openAIModel string
	// routing is the config directory's routing.json, when set.
	routing string
	// claudeCLI configures a claude CLI, which is registered as installed
	// but never answers: its binary is missing.
	claudeCLI bool
	// options are the facade's options; UsageLogger is always the recorder.
	options router.HTTPHandlerOptions
}

// apiOnlyServeWith is apiOnlyServe for the deployment d.
func apiOnlyServeWith(t *testing.T, d apiOnlyDeployment) (http.Handler, *apiOnlyUpstream, *apiOnlyUsage) {
	t.Helper()
	testfixture.ClearProviderEnv(t)
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	configDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", configDir)
	t.Setenv("MAKEWAND_API_POLICY", config.APIPolicyAllowPaid)
	t.Setenv("MAKEWAND_ENABLE_PROVIDERS", "")
	if d.routing != "" {
		if err := os.WriteFile(filepath.Join(configDir, "routing.json"), []byte(d.routing), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	upstream := &apiOnlyUpstream{}
	upstream.Server = httptest.NewServer(upstream)
	t.Cleanup(upstream.Close)

	cfg := config.DefaultConfig()
	cfg.OpenAIAPIKey = "fixture-openai-key"
	cfg.OpenAIBaseURL = upstream.URL + "/v1"
	cfg.OpenAIModel = d.openAIModel
	if d.claudeCLI {
		cfg.CLIs = []config.CLITool{{Name: "claude", BinPath: filepath.Join(t.TempDir(), "claude")}}
	}
	rtr, err := serveRouter(cfg, model.RepoTrustTrusted)
	if err != nil {
		t.Fatal(err)
	}
	usage := &apiOnlyUsage{}
	options := d.options
	options.UsageLogger = usage
	return rtr.HTTPHandler(options), upstream, usage
}

func apiOnlyPost(t *testing.T, h http.Handler, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return apiOnlyPostAs(t, h, "", path, body)
}

// apiOnlyPostAs is apiOnlyPost with the bearer token, when one is given.
func apiOnlyPostAs(t *testing.T, h http.Handler, token, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// apiOnlyModelIDs returns the model IDs GET /v1/models lists.
func apiOnlyModelIDs(t *testing.T, h http.Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: status %d body %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, entry := range list.Data {
		ids = append(ids, entry.ID)
	}
	return ids
}

func apiOnlyChat(model, prompt string, stream bool) map[string]any {
	body := map[string]any{"stream": stream, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	if model != "" {
		body["model"] = model
	}
	return body
}

// apiOnlyResponseModel returns the model a chat completion names, streamed or not.
func apiOnlyResponseModel(t *testing.T, body string, stream bool) string {
	t.Helper()
	if !stream {
		var resp struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		return resp.Model
	}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Model string `json:"model"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Model != "" {
			return chunk.Model
		}
	}
	return ""
}

func onlyUsageEntry(t *testing.T, usage *apiOnlyUsage) serverusage.Entry {
	t.Helper()
	entries := usage.take()
	if len(entries) != 1 {
		t.Fatalf("usage entries = %+v, want one", entries)
	}
	return entries[0]
}

// go-server-1021-1: Power code and fix requests reach the codex route through
// the model factory. In this shape it must call a real OpenAI model (the
// configured one) and record its price; it sent "codex-cli" upstream and
// recorded $0, so Power requests bypassed token, org and project USD budgets.
func TestServeAPIOnlyPowerRequestsCallAndBillTheOpenAIModel(t *testing.T) {
	for _, configured := range []string{"", "gpt-4o-mini", "o3-mini"} {
		h, upstream, usage := apiOnlyServe(t, configured)
		want := configured
		if want == "" {
			want = "gpt-4o" // the OpenAI provider's default model
		}
		for _, prompt := range []string{"write a function that adds two numbers", "fix the bug in this function"} {
			for _, stream := range []bool{false, true} {
				body := apiOnlyChat("", prompt, stream)
				body["mode"] = "power"
				rec := apiOnlyPost(t, h, "/v1/chat/completions", body)
				if rec.Code != http.StatusOK {
					t.Fatalf("configured=%q %q stream=%t: status %d body %s", configured, prompt, stream, rec.Code, rec.Body.String())
				}
				if models := upstream.take(); len(models) != 1 || models[0] != want {
					t.Fatalf("configured=%q %q stream=%t: upstream models %q, want [%s]", configured, prompt, stream, models, want)
				}
				entry := onlyUsageEntry(t, usage)
				price := router.EstimateCost(want, entry.PromptTokens, entry.CompletionTokens)
				if entry.CostUSD <= 0 || math.Abs(entry.CostUSD-price) > 1e-9 {
					t.Fatalf("configured=%q %q stream=%t: usage %+v billed $%v, want $%v at %s's price", configured, prompt, stream, entry, entry.CostUSD, price, want)
				}
			}
		}
	}
}

// go-server-1021-2: the documented OpenAI-family names returned 400 here,
// because they resolved to the unregistered "codex". They must reach the
// OpenAI API as named, in the form it accepts (o1 and o3-mini were sent
// max_tokens, which the API refuses), and be billed at the named model's price.
func TestServeAPIOnlyOpenAIModelNamesAreCalledAndBilledAsNamed(t *testing.T) {
	h, upstream, usage := apiOnlyServe(t, "")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, name := range []string{"gpt-4o", "o1", "o3-mini", "gpt-4o-mini"} {
			for _, stream := range []bool{false, true} {
				body := apiOnlyChat(name, "explain what a monad is", stream)
				if path == "/v1/responses" {
					body = map[string]any{"model": name, "stream": stream, "input": "explain what a monad is"}
				}
				rec := apiOnlyPost(t, h, path, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s stream=%t: status %d body %s", path, name, stream, rec.Code, rec.Body.String())
				}
				if models := upstream.take(); len(models) != 1 || models[0] != name {
					t.Fatalf("%s %s stream=%t: upstream models %q, want [%s]", path, name, stream, models, name)
				}
				if path == "/v1/chat/completions" {
					if got := apiOnlyResponseModel(t, rec.Body.String(), stream); got != name {
						t.Fatalf("%s stream=%t: response names model %q", name, stream, got)
					}
				}
				entry := onlyUsageEntry(t, usage)
				price := router.EstimateCost(name, entry.PromptTokens, entry.CompletionTokens)
				if entry.CostUSD <= 0 || math.Abs(entry.CostUSD-price) > 1e-9 {
					t.Fatalf("%s %s stream=%t: billed $%v for %d+%d tokens, want $%v at %s's price", path, name, stream, entry.CostUSD, entry.PromptTokens, entry.CompletionTokens, price, name)
				}
			}
		}
	}
	// "codex" names the route, not a model: it calls the configured model.
	rec := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat("codex", "explain what a monad is", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("codex: status %d body %s", rec.Code, rec.Body.String())
	}
	if models := upstream.take(); len(models) != 1 || models[0] != "gpt-4o" {
		t.Fatalf("codex: upstream models %q, want the configured [gpt-4o]", models)
	}
	usage.take()
}

// A model a client names must not take the openai route down for everyone.
// OpenAI rate limits are per model, and an account or OpenAI-compatible
// endpoint without a model answers 404. One 429 or three 404s for "o1" used to
// close "openai" for every request (for 30 minutes after a 429). They now
// close only o1's own route, which stops calling the upstream, and every other
// request is still served.
func TestServeAPIOnlyNamedModelFailuresStayWithThatModel(t *testing.T) {
	const prompt = "explain what a monad is"
	for _, tc := range []struct{ status, failures int }{
		{http.StatusTooManyRequests, 1}, // a rate-limit block
		{http.StatusNotFound, 3},        // the circuit breaker's threshold
	} {
		for _, failStreamed := range []bool{false, true} {
			h, upstream, usage := apiOnlyServe(t, "")
			upstream.reject("o1", tc.status)
			for range tc.failures {
				if rec := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat("o1", prompt, failStreamed)); rec.Code == http.StatusOK {
					t.Fatalf("upstream %d stream=%t: o1 was served: %s", tc.status, failStreamed, rec.Body.String())
				}
			}
			if models := upstream.take(); len(models) != tc.failures {
				t.Fatalf("upstream %d stream=%t: upstream models %q, want %d o1 calls", tc.status, failStreamed, models, tc.failures)
			}
			for _, stream := range []bool{false, true} {
				if rec := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat("o1", prompt, stream)); rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("upstream %d: o1 stream=%t after its route closed: status %d body %s, want 503", tc.status, stream, rec.Code, rec.Body.String())
				}
			}
			if models := upstream.take(); len(models) != 0 {
				t.Fatalf("upstream %d: o1 reached the upstream after its route closed: %q", tc.status, models)
			}
			for _, name := range []string{"", "openai", "codex", "gpt-4o", "gpt-4o-mini"} {
				for _, stream := range []bool{false, true} {
					rec := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat(name, prompt, stream))
					if rec.Code != http.StatusOK {
						t.Fatalf("after o1 got %d (stream=%t): model %q stream=%t: status %d body %s", tc.status, failStreamed, name, stream, rec.Code, rec.Body.String())
					}
				}
			}
			usage.take()
		}
	}
}

// Streamed and plain requests for a model the server cannot serve get the same
// error, naming the model as sent; neither reaches the upstream. An OpenAI
// name without a price is refused, since its spend could not be budgeted.
func TestServeAPIOnlyModelErrorsMatchForStreamedAndPlainRequests(t *testing.T) {
	h, upstream, _ := apiOnlyServe(t, "")
	for _, tc := range []struct{ name, want string }{
		{"Mistral-Large", `unknown model \"Mistral-Large\"`},
		{"gpt-4.1-nano", `model \"gpt-4.1-nano\" has no price on this server`},
	} {
		plain := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat(tc.name, "explain what a monad is", false))
		streamed := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat(tc.name, "explain what a monad is", true))
		if plain.Code != http.StatusBadRequest || streamed.Code != http.StatusBadRequest {
			t.Fatalf("%s: status plain=%d streamed=%d, want 400", tc.name, plain.Code, streamed.Code)
		}
		if plain.Body.String() != streamed.Body.String() {
			t.Fatalf("%s: error texts differ:\nplain:    %s\nstreamed: %s", tc.name, plain.Body.String(), streamed.Body.String())
		}
		if !strings.Contains(plain.Body.String(), tc.want) {
			t.Fatalf("%s: error %s, want %s", tc.name, plain.Body.String(), tc.want)
		}
	}
	if models := upstream.take(); len(models) != 0 {
		t.Fatalf("refused requests reached the upstream: %q", models)
	}
}

// GET /v1/models lists the OpenAI names a client can send, and each of them
// is accepted. The configured OPENAI_MODEL is listed when it is an
// OpenAI-family name, priced or not. Otherwise, as for an OpenAI-compatible
// endpoint's model or a name outside those families, it is not listed:
// clients reach it as "openai".
func TestServeAPIOnlyListsTheOpenAIModelsItAccepts(t *testing.T) {
	for _, configured := range []string{"", "gpt-4o-mini", "gpt-4.1-nano", "deepseek-chat", "chatgpt-4o-latest"} {
		h, upstream, _ := apiOnlyServe(t, configured)
		ids := apiOnlyModelIDs(t, h)
		for _, want := range []string{"openai", "gpt-4o", "gpt-4o-mini", "o1", "o3-mini"} {
			if !slices.Contains(ids, want) {
				t.Fatalf("configured=%q: models %q are missing %q", configured, ids, want)
			}
		}
		for _, unwanted := range []string{"codex-cli", "codex", "deepseek-chat", "chatgpt-4o-latest"} {
			if slices.Contains(ids, unwanted) {
				t.Fatalf("configured=%q: models %q list %q", configured, ids, unwanted)
			}
		}
		if configured == "gpt-4.1-nano" && !slices.Contains(ids, configured) {
			t.Fatalf("models %q are missing the configured %q", ids, configured)
		}
		for _, id := range ids {
			if rec := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat(id, "explain what a monad is", false)); rec.Code != http.StatusOK {
				t.Fatalf("configured=%q: listed model %q is refused: %d %s", configured, id, rec.Code, rec.Body.String())
			}
		}
		upstream.take()
		want := configured
		if want == "" {
			want = "gpt-4o"
		}
		rec := apiOnlyPost(t, h, "/v1/chat/completions", apiOnlyChat("openai", "explain what a monad is", false))
		if models := upstream.take(); rec.Code != http.StatusOK || len(models) != 1 || models[0] != want {
			t.Fatalf("configured=%q: openai got %d and called %q, want the configured model", configured, rec.Code, models)
		}
	}
}

// budgetReservations is a BudgetReserver that admits every reservation and
// records its estimate, in USD.
type budgetReservations struct {
	mu        sync.Mutex
	estimates []float64
}

func (b *budgetReservations) ReserveBudget(_ context.Context, reservation serverusage.BudgetReservation) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.estimates = append(b.estimates, float64(reservation.EstimateMicroUSD)/1e6)
	return nil
}

func (b *budgetReservations) take() []float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	estimates := b.estimates
	b.estimates = nil
	return estimates
}

// The scaled budget reservation (MAKEWAND_SCALED_BUDGET_RESERVATION=1) covers
// the model a Power request's codex route calls. Without a codex CLI the model
// factory serves that route with OPENAI_MODEL, but the reservation priced the
// codex table's "codex-cli" placeholder, at $0: it covered gpt-4o-mini where
// routing.json points openai at it, and nothing, leaving only the floor, for a
// token scoped to claude (a subscription CLI) and codex. Each request calls o1
// once; the reservation must cover that call and a call of o1 at the least
// output a Power request may ask for.
func TestServeAPIOnlyReservationCoversTheModelTheCodexRouteCalls(t *testing.T) {
	const scopedToken = "fixture-scoped-token" //nolint:gosec // G101: a test fixture, not a credential.
	authz, err := serverauth.NewAuthorizer(serverauth.Config{Tokens: []serverauth.TokenRule{{
		ID: "scoped", Token: scopedToken, Scopes: []string{serverauth.ScopeChatInvoke}, AllowedProviders: []string{"claude", "codex"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	fullOutputCall := router.EstimateCost("o1", 0, router.MaxTokensForTask(router.TaskCode))
	for _, tc := range []struct {
		name       string
		deployment apiOnlyDeployment
		token      string
	}{
		{"openai pinned to a cheaper model", apiOnlyDeployment{openAIModel: "o1", routing: `{"models":{"openai":{"cheap":"gpt-4o-mini","mid":"gpt-4o-mini","premium":"gpt-4o-mini"}}}`}, ""},
		{"token scoped to claude and codex", apiOnlyDeployment{openAIModel: "o1", claudeCLI: true, options: router.HTTPHandlerOptions{Authorizer: authz}}, scopedToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reservations := &budgetReservations{}
			deployment := tc.deployment
			deployment.options.BudgetReservationUSD = 0.0001
			deployment.options.ScaleBudgetReservation = true
			deployment.options.BudgetReserver = reservations
			h, upstream, usage := apiOnlyServeWith(t, deployment)
			for _, prompt := range []string{"write a function that adds two numbers", "fix the bug in this function"} {
				for _, stream := range []bool{false, true} {
					body := apiOnlyChat("", prompt, stream)
					body["mode"] = "power"
					rec := apiOnlyPostAs(t, h, tc.token, "/v1/chat/completions", body)
					if rec.Code != http.StatusOK {
						t.Fatalf("%q stream=%t: status %d body %s", prompt, stream, rec.Code, rec.Body.String())
					}
					if models := upstream.take(); !slices.Equal(models, []string{"o1"}) {
						t.Fatalf("%q stream=%t: upstream models %q, want [o1]", prompt, stream, models)
					}
					spent := onlyUsageEntry(t, usage).CostUSD
					reserved := reservations.take()
					if len(reserved) != 1 || reserved[0] < spent || reserved[0] < fullOutputCall {
						t.Fatalf("%q stream=%t: reserved $%v for a request that spent $%v; one o1 call can spend $%v", prompt, stream, reserved, spent, fullOutputCall)
					}
				}
			}
		})
	}
}
