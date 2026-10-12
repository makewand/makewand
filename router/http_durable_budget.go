package router

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	id := "account_" + rand.Text()
	err := opt.BudgetReserver.ReserveBudget(ctx, serverusage.BudgetReservation{ID: id, EstimateMicroUSD: serverusage.MicroUSD(estimateUSD), ExpiresAt: now.Add(15 * time.Minute), Scopes: scopes})
	if err == nil {
		entry.ReservationID = id
	}
	return err
}

// httpReservationUSD sizes the provisional budget reservation for one HTTP
// request. The configured BudgetReservationUSD is a floor and the fallback when
// no reachable API model is priced (subscription CLIs cost nothing in dollars).
// Otherwise the reservation is the worst-case spend of every provider attempt
// the request can make, priced from this Router's own pricing snapshot:
//
//	per attempt = prompt tokens × input price + per-call max output × output price
//
// A Power-mode ensemble fans out to every generator, a judge whose prompt also
// carries each generator's output, and one adaptive fallback when no generator
// produced usable content. The per-call output bound is the router's own cap
// (the provider call's max_tokens); a client-supplied max_tokens is not
// forwarded to providers, so it cannot lower the bound.
//
// A floor <= 0 keeps reservation disabled (headroom-only), as documented on
// HTTPHandlerOptions.BudgetReservationUSD.
func (r *Router) httpReservationUSD(floor float64, prepared *preparedHTTPChat) float64 {
	floor = sanitizeCost(floor)
	if floor <= 0 || r == nil || prepared == nil {
		return floor
	}
	price, ok := r.httpPriceCeiling(prepared.RequestedModel)
	if !ok {
		return floor
	}
	promptTokens := float64(estimateTokens(buildCLIPrompt(prepared.Messages, prepared.System)))
	attemptCost := func(inTokens, outTokens float64) float64 {
		return (inTokens*price.Input + outTokens*price.Output) / 1_000_000
	}
	var estimate float64
	switch {
	case prepared.RequestedModel != "":
		estimate = attemptCost(promptTokens, float64(maxTokensForPhase(taskToBuildPhase(prepared.Task))))
	case r.effectiveMode() == ModePower:
		phase := taskToBuildPhase(prepared.Task)
		maxOut := float64(maxTokensForPhase(phase))
		generators := 1
		if pe, ok := r.routingTables().powerEnsembleFor(phase); ok && len(pe.Generators) > 0 {
			generators = len(pe.Generators)
		}
		gen := float64(generators)
		estimate = gen*attemptCost(promptTokens, maxOut) + // generators
			attemptCost(promptTokens+gen*maxOut, maxOut) + // judge sees every candidate
			attemptCost(promptTokens, maxOut) // adaptive fallback after an all-empty ensemble
	default:
		estimate = attemptCost(promptTokens, float64(MaxTokensForTask(prepared.Task)))
	}
	estimate = sanitizeCost(estimate)
	if estimate > floor {
		return estimate
	}
	return floor
}

// httpPriceCeiling returns the highest input and output prices ($/MTok) among
// the models this Router can reach through API-access providers (only
// requestedModel when the client pinned one). ok is false when no reachable
// API model is priced.
func (r *Router) httpPriceCeiling(requestedModel string) (costEntry, bool) {
	names := r.registeredProviderNames()
	if requestedModel != "" {
		names = []string{requestedModel}
	}
	tables := r.routingTables()
	var ceiling costEntry
	found := false
	for _, name := range names {
		if r.getAccessType(name) != AccessAPI {
			continue
		}
		models, ok := tables.modelsFor(name)
		if !ok {
			continue
		}
		for _, modelID := range models {
			price, ok := tables.costFor(modelID)
			if !ok {
				continue
			}
			found = true
			ceiling.Input = max(ceiling.Input, sanitizeCost(price.Input))
			ceiling.Output = max(ceiling.Output, sanitizeCost(price.Output))
		}
	}
	return ceiling, found && (ceiling.Input > 0 || ceiling.Output > 0)
}

// errHTTPStreamReservationExceeded is reported to clients and audit logs when a
// stream is stopped by httpStreamSpendGuard.
const errHTTPStreamReservationExceeded = "stream stopped: estimated spend exceeded the request's budget reservation"

// httpStreamSpendGuard stops a provider stream once its estimated spend
// exceeds the request's reservation. Streaming transports rarely report usage,
// so the running spend is the priced prompt estimate plus the priced estimate
// of the output streamed so far. A zero limit, unpriced route, or a stream
// whose usage is already known (Power mode's synthetic stream) is never cut.
type httpStreamSpendGuard struct {
	limit       float64
	inputCost   float64
	outputPrice float64 // $/token
	outTokens   int
}

func newHTTPStreamSpendGuard(r *Router, limit float64, result RouteResult, messages []Message, system string, known Usage) *httpStreamSpendGuard {
	limit = sanitizeCost(limit)
	if r == nil || limit <= 0 || known.Cost > 0 || known.InputTokens > 0 || known.OutputTokens > 0 {
		return nil
	}
	if r.getAccessType(result.Actual) != AccessAPI {
		return nil
	}
	price, ok := r.routingTables().costFor(result.ModelID)
	if !ok {
		return nil
	}
	input := r.EstimateUsageForRoute(result, messages, system, "")
	return &httpStreamSpendGuard{limit: limit, inputCost: sanitizeCost(input.Cost), outputPrice: sanitizeCost(price.Output) / 1_000_000}
}

// add records streamed content and reports whether the reservation is exceeded.
func (g *httpStreamSpendGuard) add(content string) bool {
	if g == nil {
		return false
	}
	g.outTokens += estimateTokens(content)
	return g.inputCost+float64(g.outTokens)*g.outputPrice > g.limit
}
