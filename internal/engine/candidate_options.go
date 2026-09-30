package engine

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"
)

// CandidateSelectionOptions bound fan-out and runtime without changing the
// verification or human-approval requirement. A cost ceiling is soft: it stops
// new calls after an observed cost reaches the limit; it cannot refund a call.
type CandidateSelectionOptions struct {
	MaxCandidates int
	MaxConcurrent int
	Timeout       time.Duration
	MaxCostUSD    float64
}

type candidateOptionsKey struct{}

func ContextWithCandidateSelectionOptions(ctx context.Context, options CandidateSelectionOptions) context.Context {
	return context.WithValue(ctx, candidateOptionsKey{}, options)
}

func candidateSelectionOptions(ctx context.Context) (CandidateSelectionOptions, error) {
	if options, ok := ctx.Value(candidateOptionsKey{}).(CandidateSelectionOptions); ok {
		return validateCandidateOptions(options)
	}
	var options CandidateSelectionOptions
	for _, item := range []struct {
		name   string
		target *int
	}{{"MAKEWAND_CANDIDATES", &options.MaxCandidates}, {"MAKEWAND_CANDIDATE_CONCURRENCY", &options.MaxConcurrent}} {
		if value := os.Getenv(item.name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return options, fmt.Errorf("%s must be a positive integer", item.name)
			}
			*item.target = n
		}
	}
	if value := os.Getenv("MAKEWAND_CANDIDATE_TIMEOUT"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return options, fmt.Errorf("MAKEWAND_CANDIDATE_TIMEOUT must be a positive duration")
		}
		options.Timeout = duration
	}
	if value := os.Getenv("MAKEWAND_CANDIDATE_BUDGET_USD"); value != "" {
		cost, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return options, fmt.Errorf("MAKEWAND_CANDIDATE_BUDGET_USD must be a finite non-negative amount")
		}
		options.MaxCostUSD = cost
	}
	return validateCandidateOptions(options)
}

func validateCandidateOptions(options CandidateSelectionOptions) (CandidateSelectionOptions, error) {
	if options.MaxCandidates < 0 || options.MaxCandidates > 3 || options.MaxConcurrent < 0 || options.MaxConcurrent > 3 || options.Timeout < 0 || options.MaxCostUSD < 0 || math.IsNaN(options.MaxCostUSD) || math.IsInf(options.MaxCostUSD, 0) {
		return options, fmt.Errorf("candidate counts/concurrency must be 0 (default) or 1–3, with finite non-negative timeout/budget")
	}
	// Cost is learned after a call, so budgeted work runs sequentially to avoid
	// admitting several calls against the same unspent amount.
	if options.MaxCostUSD > 0 {
		options.MaxConcurrent = 1
	}
	return options, nil
}

type candidateCostBudget struct {
	mu           sync.Mutex
	limit, spent float64
}

func (b *candidateCostBudget) admit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit <= 0 || b.spent < b.limit
}
func (b *candidateCostBudget) record(cost float64) {
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spent += cost
}
