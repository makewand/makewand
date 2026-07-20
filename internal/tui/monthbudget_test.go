package tui

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMonthlyLedgerAccumulatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monthly_spend.json")
	july := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	m := LoadMonthlyLedger(path)
	m.Add(july, 3)
	m.Add(july, 4)
	if got := m.Total(july); got != 7 {
		t.Fatalf("month-to-date = %v, want 7", got)
	}

	// A reload (simulating a restart / new session) must restore the total.
	reloaded := LoadMonthlyLedger(path)
	if got := reloaded.Total(july); got != 7 {
		t.Fatalf("reloaded month-to-date = %v, want 7 (should survive restart)", got)
	}
}

func TestMonthlyLedgerRollsOverByMonth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monthly_spend.json")
	m := LoadMonthlyLedger(path)

	july := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	m.Add(july, 5)
	if got := m.Total(july); got != 5 {
		t.Fatalf("july total = %v, want 5", got)
	}

	august := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if got := m.Total(august); got != 0 {
		t.Fatalf("august total = %v, want 0 (month rolled over)", got)
	}
	m.Add(august, 2)
	if got := m.Total(august); got != 2 {
		t.Fatalf("august total after add = %v, want 2", got)
	}
}

func TestMonthlyLedgerBudgetStatus(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	m := LoadMonthlyLedger(filepath.Join(t.TempDir(), "s.json"))

	m.Add(now, 9) // 90% of a $10 budget → warning
	if s := m.BudgetStatus(now, 10); s.Level != BudgetWarning {
		t.Fatalf("level = %v, want BudgetWarning at 90%%", s.Level)
	}
	m.Add(now, 1) // 100% → exceeded
	if s := m.BudgetStatus(now, 10); s.Level != BudgetExceeded {
		t.Fatalf("level = %v, want BudgetExceeded at 100%%", s.Level)
	}
	// A zero budget disables the check.
	if s := m.BudgetStatus(now, 0); s.Level != BudgetOK {
		t.Fatalf("level = %v, want BudgetOK when budget disabled", s.Level)
	}
}

// TestMonthlyLedgerNilSafe guards the value-receiver App paths that may hold a
// nil ledger in tests.
func TestMonthlyLedgerNilSafe(t *testing.T) {
	var m *MonthlyLedger
	if got := m.Total(time.Now()); got != 0 {
		t.Fatalf("nil ledger total = %v, want 0", got)
	}
	if s := m.BudgetStatus(time.Now(), 10); s.Level != BudgetOK {
		t.Fatalf("nil ledger status = %v, want BudgetOK", s.Level)
	}
	m.Add(time.Now(), 5) // must not panic
}
