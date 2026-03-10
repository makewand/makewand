package tui

import (
	"fmt"
	"strings"

	"github.com/makewand/makewand/internal/i18n"
)

// CostTracker tracks API costs across providers.
type CostTracker struct {
	entries []costEntry
}

// BudgetLevel describes current spend pressure against the configured budget.
type BudgetLevel int

const (
	BudgetOK BudgetLevel = iota
	BudgetWarning
	BudgetExceeded
)

// BudgetStatus summarizes current budget utilization.
type BudgetStatus struct {
	Level   BudgetLevel
	Total   float64
	Budget  float64
	Percent float64
}

type costEntry struct {
	provider       string
	cost           float64
	isSubscription bool
	inputTokens    int
	outputTokens   int
}

// NewCostTracker creates a new cost tracker.
func NewCostTracker() *CostTracker {
	return &CostTracker{}
}

// Add records a cost entry (backward compatible).
func (c *CostTracker) Add(provider string, cost float64) {
	c.entries = append(c.entries, costEntry{provider: provider, cost: cost})
}

// AddWithTokens records a cost entry with token details and subscription info.
func (c *CostTracker) AddWithTokens(provider string, cost float64, inputTokens, outputTokens int, isSubscription bool) {
	c.entries = append(c.entries, costEntry{
		provider:       provider,
		cost:           cost,
		isSubscription: isSubscription,
		inputTokens:    inputTokens,
		outputTokens:   outputTokens,
	})
}

func (c *CostTracker) Snapshot() []costEntry {
	return append([]costEntry(nil), c.entries...)
}

func (c *CostTracker) Restore(entries []costEntry) {
	c.entries = append([]costEntry(nil), entries...)
}

// SessionTotal returns the total cost for the current session.
func (c *CostTracker) SessionTotal() float64 {
	var total float64
	for _, e := range c.entries {
		total += e.cost
	}
	return total
}

// ByProvider returns costs grouped by provider.
func (c *CostTracker) ByProvider() map[string]float64 {
	m := make(map[string]float64)
	for _, e := range c.entries {
		m[e.provider] += e.cost
	}
	return m
}

// RequestCount returns the number of requests for a provider.
func (c *CostTracker) RequestCount(provider string) int {
	count := 0
	for _, e := range c.entries {
		if e.provider == provider {
			count++
		}
	}
	return count
}

// TokensByProvider returns total input+output tokens for a provider.
func (c *CostTracker) TokensByProvider(provider string) (int, int) {
	var input, output int
	for _, e := range c.entries {
		if e.provider == provider {
			input += e.inputTokens
			output += e.outputTokens
		}
	}
	return input, output
}

// IsSubscription returns true if any entry for the provider is subscription-based.
func (c *CostTracker) IsSubscription(provider string) bool {
	for _, e := range c.entries {
		if e.provider == provider && e.isSubscription {
			return true
		}
	}
	return false
}

// CheckBudget returns a warning string if the session total approaches or exceeds the budget.
// Returns empty string if budget is zero (disabled) or spending is below 80%.
func (c *CostTracker) CheckBudget(budget float64) string {
	status := c.BudgetStatus(budget)
	switch status.Level {
	case BudgetExceeded:
		return fmt.Sprintf("Budget exceeded: $%.2f / $%.2f (%.0f%%)", status.Total, status.Budget, status.Percent)
	case BudgetWarning:
		return fmt.Sprintf("Budget warning: $%.2f / $%.2f (%.0f%%)", status.Total, status.Budget, status.Percent)
	default:
		return ""
	}
}

// BudgetStatus returns a structured spend status for routing and UI policies.
func (c *CostTracker) BudgetStatus(budget float64) BudgetStatus {
	if budget <= 0 {
		return BudgetStatus{Level: BudgetOK}
	}
	total := c.SessionTotal()
	pct := total / budget * 100
	if pct >= 100 {
		return BudgetStatus{
			Level:   BudgetExceeded,
			Total:   total,
			Budget:  budget,
			Percent: pct,
		}
	}
	if pct >= 80 {
		return BudgetStatus{
			Level:   BudgetWarning,
			Total:   total,
			Budget:  budget,
			Percent: pct,
		}
	}
	return BudgetStatus{
		Level:   BudgetOK,
		Total:   total,
		Budget:  budget,
		Percent: pct,
	}
}

func formatTokenCount(tokens int) string {
	if tokens >= 1000 {
		return fmt.Sprintf("%dK", tokens/1000)
	}
	return fmt.Sprintf("%d", tokens)
}

// View renders the cost panel.
func (c *CostTracker) View(width int) string {
	msg := i18n.Msg()
	var b strings.Builder

	b.WriteString(costStyle.Render(fmt.Sprintf("┌─ %s ─", msg.CostSession)))
	b.WriteString("\n")

	byProvider := c.ByProvider()
	providers := []struct {
		name  string
		label string
	}{
		{"gemini", "Gemini"},
		{"claude", "Claude"},
		{"codex", "Codex"},
	}

	for _, p := range providers {
		cost, ok := byProvider[p.name]
		if !ok {
			continue
		}

		var costStr string
		if c.IsSubscription(p.name) {
			// Subscription: show request count and token estimate
			reqCount := c.RequestCount(p.name)
			inTok, outTok := c.TokensByProvider(p.name)
			totalTok := inTok + outTok
			costStr = fmt.Sprintf(msg.CostRequests, reqCount, formatTokenCount(totalTok))
		} else if cost == 0 {
			costStr = msg.CostFree
		} else {
			costStr = fmt.Sprintf("$%.2f", cost)
		}
		b.WriteString(fmt.Sprintf("│ %-8s %s\n", p.label+":", costStr))
	}

	total := c.SessionTotal()
	b.WriteString("│ ─────────────────\n")
	b.WriteString(fmt.Sprintf("│ %-8s %8s\n", msg.CostTotal+":", fmt.Sprintf("$%.2f", total)))
	b.WriteString("└─────────────────┘")

	return b.String()
}
