package router

import "strings"

// EstimateUsageForRoute returns a conservative token estimate for a completed
// response when the transport did not provide native streaming usage. Dollar
// cost is estimated only for API-backed providers, priced as the model the
// route actually called (apiBilledModelID): at its table price or, for a model
// without a pricing entry (or one the route cannot name), at the same
// conservative fallback rate non-streaming completions of unpriced models are
// billed at (priceCompletionWithStatus). The estimate is never marked measured
// (MeasuredTokens and MeasuredCost stay false). Subscription requests keep zero
// direct cost while still contributing request and token counts.
func (r *Router) EstimateUsageForRoute(result RouteResult, messages []Message, system, output string) Usage {
	return r.estimateUsageForCall(result, messages, system, output, 0)
}

// estimateUsageForCall is EstimateUsageForRoute for one provider call whose
// max_tokens is known: its output estimate is capped at that output cap
// (estimateOutputTokens). maxTokens <= 0 leaves it uncapped.
func (r *Router) estimateUsageForCall(result RouteResult, messages []Message, system, output string, maxTokens int) Usage {
	inputTokens, outputTokens := estimateCallTokens(messages, system, output, maxTokens)
	usage := Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Model:        result.ModelID,
		Provider:     result.Actual,
	}
	if r == nil || r.getAccessType(result.Actual) != AccessAPI {
		return usage
	}
	cost, _ := priceCompletionWithStatus(r.routingTables(), apiBilledModelID(result), usage.InputTokens, usage.OutputTokens)
	usage.Cost = sanitizeCost(cost)
	return usage
}

// estimateCallTokens estimates the prompt and output tokens of a provider call
// whose transport reported no usage, from the text it was sent and returned.
func estimateCallTokens(messages []Message, system, output string, maxTokens int) (inputTokens, outputTokens int) {
	return estimateTokens(buildCLIPrompt(messages, system)), estimateOutputTokens(output, maxTokens)
}

// estimateOutputTokens estimates the output tokens of a provider call that
// reported none: the billing heuristic estimateTokens, which overcounts short
// words (a reply of exactly max_tokens one-token words by 30%), capped at the
// call's max_tokens, which a provider honoring its output cap cannot bill
// more than. Text whose streamTokenFloor exceeds the cap (a provider ignoring
// it) is capped at that lower estimate instead, since the heuristic bills a
// long whitespace-free run many times its tokens (a 64-character separator
// line as 18). maxTokens <= 0 means the cap is unknown.
func estimateOutputTokens(output string, maxTokens int) int {
	estimate := estimateTokens(output)
	if maxTokens <= 0 || estimate <= maxTokens {
		return estimate
	}
	var floor streamTokenFloor
	floor.add(output)
	return min(estimate, max(maxTokens, floor.tokens()))
}

// apiBilledModelID returns the model an API route is billed at: the route's
// model or, when routing named none, the model its provider calls on its own
// (providerModelReporter). Empty means unknown, billed at the unpriced
// fallback rate. Spend estimates, budget reservations (httpPriceCeiling) and
// the stream spend guard all price a route through it.
func apiBilledModelID(result RouteResult) string {
	if modelID := strings.TrimSpace(result.ModelID); modelID != "" {
		return modelID
	}
	if result.Provider == nil {
		return ""
	}
	return reportedModelID(result.Provider, "")
}

// apiEstimatePrice returns the $/MTok rate used to estimate spend on an
// API-access model: its pricing entry or, when it has none, the unpriced
// fallback rate. priced is false for the fallback.
func (r *Router) apiEstimatePrice(modelID string) (price costEntry, priced bool) {
	if entry, ok := r.routingTables().costFor(modelID); ok {
		return entry, true
	}
	return unpricedFallbackCost(), false
}

// unpricedFallbackCost is the conservative rate billed for API models without a
// pricing entry (conservativeInputCostPer1M / conservativeOutputCostPer1M, the
// rate priceCompletionWithStatus charges), so streamed estimates and budget
// reservations price such models exactly like non-streaming completions.
func unpricedFallbackCost() costEntry {
	return costEntry{Input: conservativeInputCostPer1M, Output: conservativeOutputCostPer1M}
}
