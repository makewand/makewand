package router

// EstimateUsageForRoute returns a conservative token estimate for a completed
// response when the transport did not provide native streaming usage. Dollar
// cost is estimated only for API-backed providers; subscription requests keep
// zero direct cost while still contributing request and token counts.
func (r *Router) EstimateUsageForRoute(result RouteResult, messages []Message, system, output string) Usage {
	usage := Usage{
		InputTokens:  estimateTokens(buildCLIPrompt(messages, system)),
		OutputTokens: estimateTokens(output),
		Model:        result.ModelID,
		Provider:     result.Actual,
	}
	if r == nil || r.getAccessType(result.Actual) != AccessAPI {
		return usage
	}
	if price, ok := r.routingTables().costFor(result.ModelID); ok {
		usage.Cost = float64(usage.InputTokens)*price.Input/1_000_000 +
			float64(usage.OutputTokens)*price.Output/1_000_000
	}
	return usage
}
