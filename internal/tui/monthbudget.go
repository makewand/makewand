package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/makewand/makewand/internal/config"
)

// monthlyLedgerFile is the on-disk shape of the monthly spend ledger.
type monthlyLedgerFile struct {
	Month    string  `json:"month"` // "2006-01"
	SpentUSD float64 `json:"spent_usd"`
}

// MonthlyLedger tracks pay-as-you-go spend for the current calendar month,
// persisted to disk so the budget reflects a true month-to-date total that
// survives /clear, new chat sessions, and restarts — unlike the session cost
// panel, which is per-conversation. It rolls over automatically when the
// calendar month changes.
type MonthlyLedger struct {
	mu    sync.Mutex
	path  string
	month string
	spent float64
}

func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }

// monthlyLedgerPath returns the shared ledger location, or "" when the config
// dir is unavailable (in which case the ledger stays in memory only).
func monthlyLedgerPath() string {
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "monthly_spend.json")
}

// LoadMonthlyLedger reads the ledger at path. A missing or invalid file yields
// an empty ledger rather than an error.
func LoadMonthlyLedger(path string) *MonthlyLedger {
	m := &MonthlyLedger{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var f monthlyLedgerFile
	if json.Unmarshal(data, &f) == nil {
		m.month = f.Month
		m.spent = f.SpentUSD
	}
	return m
}

// Add records pay-as-you-go cost against the month of `now`, rolling the ledger
// over first if the calendar month has changed. Zero/negative costs are ignored.
func (m *MonthlyLedger) Add(now time.Time, cost float64) {
	if m == nil || cost <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverLocked(now)
	m.spent += cost
	m.persistLocked()
}

// Total returns month-to-date spend for the month of `now` (0 after a rollover).
func (m *MonthlyLedger) Total(now time.Time) float64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverLocked(now)
	return m.spent
}

// BudgetStatus reports spend pressure for the current month against budget.
func (m *MonthlyLedger) BudgetStatus(now time.Time, budget float64) BudgetStatus {
	return budgetStatusFor(m.Total(now), budget)
}

func (m *MonthlyLedger) rolloverLocked(now time.Time) {
	if key := monthKey(now); m.month != key {
		m.month = key
		m.spent = 0
	}
}

func (m *MonthlyLedger) persistLocked() {
	if m.path == "" {
		return
	}
	data, err := json.Marshal(monthlyLedgerFile{Month: m.month, SpentUSD: m.spent})
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.path), 0o700)
	_ = os.WriteFile(m.path, data, 0o600)
}
