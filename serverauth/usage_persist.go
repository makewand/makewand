package serverauth

import "time"

// UsageCounters is a serializable snapshot of one token's accounting counters.
// It exists so the in-memory quota/spend state can be persisted across restarts
// (the counters otherwise reset when the process exits). Window-start fields are
// preserved so that a restore into a new day/month self-heals: the next
// Check/Record call resets a stale window before enforcing it.
type UsageCounters struct {
	TokenID          string
	QuotaWindowStart time.Time
	QuotaWindowCount int
	QuotaDayStart    time.Time
	QuotaDayCount    int
	CostDayStart     time.Time
	CostDaySpent     float64
	CostMonthStart   time.Time
	CostMonthSpent   float64
}

// isEmpty reports whether the counters carry no accrued usage worth persisting.
func (c UsageCounters) isEmpty() bool {
	return c.QuotaWindowCount == 0 && c.QuotaDayCount == 0 &&
		c.CostDaySpent == 0 && c.CostMonthSpent == 0
}

// exportUsage returns the grant's current counters keyed by token id, and false
// when the grant has no accrued usage worth persisting.
func (g *Grant) exportUsage() (UsageCounters, bool) {
	if g == nil || g.usage == nil {
		return UsageCounters{}, false
	}
	u := g.usage
	u.mu.Lock()
	defer u.mu.Unlock()
	c := UsageCounters{
		TokenID:          g.tokenID,
		QuotaWindowStart: u.quotaWindowStart,
		QuotaWindowCount: u.quotaWindowCount,
		QuotaDayStart:    u.quotaDayStart,
		QuotaDayCount:    u.quotaDayCount,
		CostDayStart:     u.costDayStart,
		CostDaySpent:     u.costDaySpent,
		CostMonthStart:   u.costMonthStart,
		CostMonthSpent:   u.costMonthSpent,
	}
	if c.isEmpty() {
		return UsageCounters{}, false
	}
	return c, true
}

// importUsage loads persisted counters into the grant, overwriting whatever it
// currently holds. Intended for one-shot restore at startup before the grant
// has served any request. Stale windows self-heal on the next Check/Record.
func (g *Grant) importUsage(c UsageCounters) {
	if g == nil || g.usage == nil {
		return
	}
	u := g.usage
	u.mu.Lock()
	defer u.mu.Unlock()
	u.quotaWindowStart = c.QuotaWindowStart
	u.quotaWindowCount = c.QuotaWindowCount
	u.quotaDayStart = c.QuotaDayStart
	u.quotaDayCount = c.QuotaDayCount
	u.costDayStart = c.CostDayStart
	u.costDaySpent = c.CostDaySpent
	u.costMonthStart = c.CostMonthStart
	u.costMonthSpent = c.CostMonthSpent
}
