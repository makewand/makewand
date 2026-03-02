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

type costEntry struct {
	provider string
	cost     float64
}

// NewCostTracker creates a new cost tracker.
func NewCostTracker() *CostTracker {
	return &CostTracker{}
}

// Add records a cost entry.
func (c *CostTracker) Add(provider string, cost float64) {
	c.entries = append(c.entries, costEntry{provider: provider, cost: cost})
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
		{"openai", "OpenAI"},
		{"ollama", "Ollama"},
	}

	for _, p := range providers {
		cost, ok := byProvider[p.name]
		if !ok {
			continue
		}
		var costStr string
		if cost == 0 {
			if p.name == "ollama" {
				costStr = msg.CostLocal
			} else {
				costStr = msg.CostFree
			}
		} else {
			costStr = fmt.Sprintf("$%.2f", cost)
		}
		b.WriteString(fmt.Sprintf("│ %-8s %8s\n", p.label+":", costStr))
	}

	total := c.SessionTotal()
	b.WriteString("│ ─────────────────\n")
	b.WriteString(fmt.Sprintf("│ %-8s %8s\n", msg.CostTotal+":", fmt.Sprintf("$%.2f", total)))
	b.WriteString("└─────────────────┘")

	return b.String()
}
