package router

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/makewand/makewand/serverauth"
	"github.com/makewand/makewand/serverusage"
)

func (opt HTTPHandlerOptions) reserveDurableBudget(ctx context.Context, grant *serverauth.Grant, entry *serverusage.Entry, estimateUSD float64) error {
	if entry == nil {
		return fmt.Errorf("durable accounting entry is missing")
	}
	now := entry.Timestamp.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
		entry.Timestamp = now
	}
	month := now.Format("2006-01")
	var scopes []serverusage.BudgetScope
	add := func(kind, id, period string, cap float64) {
		if cap > 0 {
			scopes = append(scopes, serverusage.BudgetScope{Kind: kind, ID: id, Period: period, LimitMicroUSD: serverusage.MicroUSD(cap)})
		}
	}
	if grant != nil {
		add("token_day", grant.TokenID(), now.Format("2006-01-02"), grant.MaxCostUSDPerDay())
		add("token_month", grant.TokenID(), month, grant.MaxCostUSDPerMonth())
		orgID := strings.TrimSpace(grant.OrganizationID())
		if projectID := strings.TrimSpace(grant.ProjectID()); projectID != "" {
			if opt.TeamStore == nil {
				return fmt.Errorf("project budget store unavailable")
			}
			project, err := opt.TeamStore.GetProject(projectID)
			if err != nil {
				return err
			}
			if project == nil {
				return fmt.Errorf("project %q missing", projectID)
			}
			parent := strings.TrimSpace(project.OrganizationID)
			if orgID != "" && parent != "" && orgID != parent {
				return &httpStatusError{Status: http.StatusForbidden, Code: "scope_conflict", Message: "token project and organization disagree"}
			}
			if orgID == "" {
				orgID = parent
				entry.OrganizationID = orgID
			}
			add("project_month", projectID, month, project.MonthlyBudgetUSD)
		}
		if orgID != "" {
			if opt.TeamStore == nil {
				return fmt.Errorf("organization budget store unavailable")
			}
			org, err := opt.TeamStore.GetOrganization(orgID)
			if err != nil {
				return err
			}
			if org == nil {
				return fmt.Errorf("organization %q missing", orgID)
			}
			add("org_month", orgID, month, org.MonthlyBudgetUSD)
		}
	}
	// Correlation IDs can repeat and are client-controlled. Only this trusted,
	// unpredictable identity owns the reservation and exactly-once settlement.
	// A request no cap applies to reserves too: without the ID, a usage
	// callback repeated after an ambiguous commit would be recorded twice.
	id := "account_" + rand.Text()
	err := opt.BudgetReserver.ReserveBudget(ctx, serverusage.BudgetReservation{ID: id, EstimateMicroUSD: serverusage.MicroUSD(estimateUSD), ExpiresAt: now.Add(15 * time.Minute), Scopes: scopes})
	if err == nil {
		entry.ReservationID = id
	}
	return err
}

// httpReservationUSD sizes the provisional budget reservation for one HTTP
// request. The configured BudgetReservationUSD is a floor and the fallback when
// no API provider is reachable (subscription CLIs cost nothing in dollars).
// Otherwise the reservation is the worst-case spend of every provider attempt
// the request can make, priced from this Router's own pricing snapshot (an
// unpriced API model at the unpriced fallback rate, see httpPriceCeiling):
//
//	per attempt = prompt tokens × input price + per-call max output × output price
//
// A Power-mode ensemble fans out to every generator, a judge whose prompt also
// carries each generator's output, and one adaptive fallback when no generator
// produced usable content. The per-call output bound is the router's own cap
// (the provider call's max_tokens, httpCallMaxTokens); a client-supplied
// max_tokens is not forwarded to providers, so it cannot lower the bound.
//
// A floor <= 0 keeps reservation disabled (headroom-only), as documented on
// HTTPHandlerOptions.BudgetReservationUSD.
func (r *Router) httpReservationUSD(floor float64, prepared *preparedHTTPChat) float64 {
	floor = sanitizeCost(floor)
	if floor <= 0 || r == nil || prepared == nil {
		return floor
	}
	price, ok := r.httpPriceCeiling()
	if !ok {
		return floor
	}
	promptTokens := float64(estimateTokens(buildCLIPrompt(prepared.Messages, prepared.System)))
	attemptCost := func(inTokens, outTokens float64) float64 {
		return (inTokens*price.Input + outTokens*price.Output) / 1_000_000
	}
	maxOut := float64(r.httpCallMaxTokens(prepared.RequestedModel, prepared.Task))
	estimate := attemptCost(promptTokens, maxOut)
	if prepared.RequestedModel == "" && r.effectiveMode() == ModePower {
		generators := 1
		if pe, ok := r.routingTables().powerEnsembleFor(taskToBuildPhase(prepared.Task)); ok && len(pe.Generators) > 0 {
			generators = len(pe.Generators)
		}
		gen := float64(generators)
		estimate = gen*attemptCost(promptTokens, maxOut) + // generators
			attemptCost(promptTokens+gen*maxOut, maxOut) + // judge sees every candidate
			attemptCost(promptTokens, maxOut) // adaptive fallback after an all-empty ensemble
	}
	estimate = sanitizeCost(estimate)
	if estimate > floor {
		return estimate
	}
	return floor
}

// httpPriceCeiling returns the highest input and output prices ($/MTok) among
// the models this Router's API-access providers, registered or factory-built
// (billedProviderNames), are billed at: every model routing can call through
// them, as the provider serving the call names it (apiBilledModels).
// It takes no provider a client pinned: a pinned request falls back to the
// other registered providers when its own is unavailable or fails
// (RouteProvider, chatWithFallbackCandidates) - a client asking for
// "claude-sonnet" pins the "claude" subscription CLI and is served by its
// "claude-api" sibling - so a pin cannot narrow the ceiling. A model without
// a pricing entry, or one its provider does not name, counts at the unpriced
// fallback rate its usage is billed at, so the ceiling never falls below the
// price the estimate and the stream spend guard apply to a route. ok is false
// only when no API provider is reachable.
func (r *Router) httpPriceCeiling() (costEntry, bool) {
	var ceiling costEntry
	found := false
	for _, name := range r.billedProviderNames() {
		if r.getAccessType(name) != AccessAPI {
			continue
		}
		for _, modelID := range r.apiBilledModels(name) {
			price, _ := r.apiEstimatePrice(modelID)
			found = true
			ceiling.Input = max(ceiling.Input, sanitizeCost(price.Input))
			ceiling.Output = max(ceiling.Output, sanitizeCost(price.Output))
		}
	}
	return ceiling, found && (ceiling.Input > 0 || ceiling.Output > 0)
}

// billedProviderNames returns the providers a request can be billed through:
// the registered ones and those only a factory builds, which Power ensembles
// and build phases resolve on demand, such as the codex route of an
// API-key-only deployment, whose table routing.json can point at a model
// pricier than any registered provider's.
func (r *Router) billedProviderNames() []string {
	names := r.registeredProviderNames()
	r.providerMu.Lock()
	defer r.providerMu.Unlock()
	for name := range r.factories {
		if r.isProviderAllowed(name) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// apiBilledModels returns the models requests routed to provider name are
// billed at (billedModelID): each tier model routing requests for it - from
// its own model table entry or, for an "<name>-api" sibling, from the base
// provider's entry that candidate and legacy routing resolve - plus the model
// the provider calls on its own whenever routing requests none (no table
// entry of its own, as for openai or a pinned "-api" sibling, or an empty
// tier). An empty ID is an unknown model.
func (r *Router) apiBilledModels(name string) []string {
	tables := r.routingTables()
	models, direct := tables.modelsFor(name)
	if !direct {
		models, _ = tables.modelsFor(baseProviderName(name))
	}
	callsOwnModel := !direct
	var billedModels []string
	for _, tier := range []ModelTier{TierCheap, TierMid, TierPremium} {
		if models[tier] == "" {
			callsOwnModel = true
			continue
		}
		billedModels = append(billedModels, r.billedModelID(name, models[tier]))
	}
	if callsOwnModel {
		billedModels = append(billedModels, r.billedModelID(name, ""))
	}
	return billedModels
}

// billedModelID returns the model a call routed to provider name, requesting
// modelID, is billed at: the model the provider execution resolves for them
// (resolveProvider) calls, found without building one. That is the instance
// cached for them (on this Router or, for a per-request view, its root) or the
// registered provider execution reuses, as it reports the model
// (reportedModelID), or else the model resolution the provider's factory
// declared (RegisterProviderFactoryWithModel). Failing those it is modelID,
// unless that is a "-cli" placeholder: such an ID names a subscription CLI's
// own default, no API model, so the model the route calls is unknown ("").
// Building the provider instead could exec a CLI or reach the network.
func (r *Router) billedModelID(name, modelID string) string {
	key := providerKey{name: strings.ToLower(strings.TrimSpace(name)), modelID: modelID}
	r.providerMu.Lock()
	provider, found := r.providerCache[key]
	if !found {
		provider, found = r.reusedRegisteredLocked(key)
	}
	factoryModel := r.factoryModels[key.name]
	root := r.cacheRoot
	r.providerMu.Unlock()
	if !found && root != nil {
		root.providerMu.Lock()
		provider, found = root.providerCache[key]
		root.providerMu.Unlock()
	}
	switch {
	case found:
		return reportedModelID(provider, modelID)
	case factoryModel != nil:
		return strings.TrimSpace(factoryModel(modelID))
	case strings.HasSuffix(modelID, "-cli"):
		return ""
	default:
		return modelID
	}
}

// errHTTPStreamReservationExceeded is reported to clients and audit logs when a
// stream is stopped by httpStreamSpendGuard.
const errHTTPStreamReservationExceeded = "stream stopped: estimated spend exceeded the request's budget reservation"

// httpStreamSpendGuard stops a provider stream that has outrun the request's
// reservation. Streaming transports rarely report usage, so the guard prices
// the prompt estimate and a lower estimate of the tokens streamed so far
// (streamTokenFloor) at the route's billed model price - an unpriced API model
// at the unpriced fallback rate it is billed at. The reservation covers the
// provider call's max_tokens at a price no lower than the route's, so a reply
// that stays within max_tokens is never cut; only a stream that exceeds what
// the reservation covers (a provider ignoring its output cap) is, once even the
// vocabulary that packs its text densest would encode it to more tokens. A
// route whose own vocabulary packs the text less densely is stopped that much
// later: a flood of dots, which DeepSeek-V3 packs 128 to a token, at twice the
// covered tokens on GPT-4o (64 to a token). A zero limit, a non-API route, a
// route whose output is free, or a stream whose usage is already known (Power
// mode's synthetic stream) is never cut.
type httpStreamSpendGuard struct {
	coveredTokens float64 // output tokens the reservation covers at the route's price
	streamed      streamTokenFloor
}

func newHTTPStreamSpendGuard(r *Router, limit float64, result RouteResult, messages []Message, system string, known Usage) *httpStreamSpendGuard {
	limit = sanitizeCost(limit)
	if r == nil || limit <= 0 || known.Cost > 0 || known.InputTokens > 0 || known.OutputTokens > 0 {
		return nil
	}
	if r.getAccessType(result.Actual) != AccessAPI {
		return nil
	}
	price, _ := r.apiEstimatePrice(apiBilledModelID(result))
	outputPrice := sanitizeCost(price.Output) / 1_000_000
	if outputPrice <= 0 {
		return nil
	}
	input := r.EstimateUsageForRoute(result, messages, system, "")
	covered := (limit - sanitizeCost(input.Cost)) / outputPrice
	// The reservation sums the same prices in another order; the relative slack
	// keeps a reply of exactly the covered size from tripping on float rounding.
	return &httpStreamSpendGuard{coveredTokens: math.Floor(covered + math.Abs(covered)*1e-9)}
}

// add records streamed content and reports whether the reservation is exceeded.
func (g *httpStreamSpendGuard) add(content string) bool {
	if g == nil {
		return false
	}
	g.streamed.add(content)
	return float64(g.streamed.tokens()) > g.coveredTokens
}

// streamTokenFloor accumulates a lower estimate of the tokens a text streamed
// in arbitrary chunks encodes to: a count the tokens of real text do not fall
// below. Tokenizers do not merge across whitespace - GPT-4o's, joining lines of
// slashes across their newlines, is the one exception among the vocabularies
// below - so every whitespace-separated word is at least one token. Within a
// word, the first token may be a whole long word of up to 16 ASCII characters;
// past it, real text averages no more than 8 bytes of UTF-8 per token, a
// dense-script character (Han, Kana, Hangul, Thai) is at least a quarter of
// one, an emoji or another character outside the Basic Multilingual Plane at
// least one (a flag's two regional indicators one together). A run of one
// repeated character packs more: past its first character, each repeat of a
// character listed in runMergeLimits counts 1/limit of a token (a line of 64
// dashes is one token, one of 64 box-drawing characters four), and a run of one
// repeated whitespace character counts a token per 512 spaces or 32 other
// whitespace characters. These limits are the densest packing of such runs in
// the o200k_base and cl100k_base (GPT-4o, GPT-4), Llama 3, Qwen 2.5 and 3,
// DeepSeek-V3, Gemma 2 and 3, Mistral Nemo, GLM-4.5 and Grok-1 vocabularies.
// Words, runs and UTF-8 sequences split across chunks count once. The heuristic
// estimateTokens used for billing overestimates short words and long single
// tokens, so it would cut legitimate replies before they reach max_tokens; a
// flat 16 characters per token for long words instead let a runaway of emoji
// stream 16 times what the reservation covers.
type streamTokenFloor struct {
	words      int                   // tokens of completed words and whitespace runs
	weight     int                   // weight of the word in progress, in 1/64 tokens, without the run in progress
	run        rune                  // character of the run in progress
	runLen     int                   // its repeats so far, 0 for none
	partial    [utf8.UTFMax - 1]byte // start of a UTF-8 sequence the next chunk completes
	partialLen int
}

// runMergeLimits lists the characters whose runs some of those vocabularies
// merge into fewer tokens than their weight (runeTokenWeight) allows, by the
// most characters of a run one token holds there, rounded up to a power of
// two. Letters, digits and characters outside the Basic Multilingual Plane are
// left out although a few vocabularies merge runs of some of them
// ("xxxxxxxxxxxxxxxx", "🔥🔥"): they keep their weight, so a runaway of
// letters or emoji is stopped close to what the reservation covers.
var runMergeLimits = [...]struct {
	limit int
	chars string
}{
	{128, "."},
	{64, "#%*-/=_"},
	{32, "+:\\~▁"},
	{16, "\x00!;<?^—―…─━▍□●\u037e\ufffd"},
	{8, "·ـ–═█▬★☆♀・！／＿￣\u200b\u2800"},
	{4, "’•№↑↓⭐、。ー﹍＊－．＝？～･\u1fef\ufeff"},
}

func (f *streamTokenFloor) add(text string) {
	if f.partialLen > 0 {
		text = string(f.partial[:f.partialLen]) + text
		f.partialLen = 0
	}
	for i := 0; i < len(text); {
		if !utf8.FullRuneInString(text[i:]) {
			f.partialLen = copy(f.partial[:], text[i:])
			return
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		if f.runLen > 0 && r == f.run {
			f.runLen++
			continue
		}
		f.endRun()
		if unicode.IsSpace(r) {
			f.words += wordTokenFloor(f.weight)
			f.weight = 0
		}
		f.run, f.runLen = r, 1
	}
}

// endRun folds the run in progress into the word's weight or, for a run of
// whitespace, into the completed tokens.
func (f *streamTokenFloor) endRun() {
	if f.runLen == 0 {
		return
	}
	if unicode.IsSpace(f.run) {
		f.words += whitespaceRunTokens(f.run, f.runLen)
	} else {
		f.weight += runTokenWeight(f.run, f.runLen)
	}
	f.runLen = 0
}

func (f *streamTokenFloor) tokens() int {
	words, weight := f.words, f.weight
	if f.runLen > 0 {
		if unicode.IsSpace(f.run) {
			words += whitespaceRunTokens(f.run, f.runLen)
		} else {
			weight += runTokenWeight(f.run, f.runLen)
		}
	}
	return words + wordTokenFloor(weight)
}

// runTokenWeight is the weight, in 1/64 tokens, of a run of n of the non-space
// character r: n times its weight or, for a character in runMergeLimits, its
// weight plus a 1/limit share of a token per repeat, if less.
func runTokenWeight(r rune, n int) int {
	weight := runeTokenWeight(r)
	if n > 1 {
		for _, merge := range runMergeLimits {
			if strings.ContainsRune(merge.chars, r) {
				return min(n*weight, weight+((n-1)*64+merge.limit-1)/merge.limit)
			}
		}
	}
	return n * weight
}

// whitespaceRunTokens is the fewest tokens a run of n of the whitespace
// character r encodes to: one per 512 spaces, or per 32 of another whitespace
// character (newline, tab, ideographic space), the most one token holds.
func whitespaceRunTokens(r rune, n int) int {
	if r == ' ' {
		return n / 512
	}
	return n / 32
}

// runeTokenWeight is the share of a token, in 1/64 tokens, one non-space
// character of a word counts for (see streamTokenFloor).
func runeTokenWeight(r rune) int {
	switch {
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Thai):
		return 16
	case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicator: a pair is one flag
		return 32
	case r > 0xFFFF:
		return 64
	default:
		return 8 * utf8.RuneLen(r) // 8 bytes a token; an invalid byte decodes as a 3-byte U+FFFD
	}
}

// wordTokenFloor is the fewest tokens one whitespace-free word of the given
// weight (in 1/64 tokens) encodes to: one, or one fewer than its weight in
// tokens, rounded up, since its first token may be a 16-character word.
func wordTokenFloor(weight int) int {
	if weight <= 0 {
		return 0
	}
	return max(1, (weight-1)/64)
}
