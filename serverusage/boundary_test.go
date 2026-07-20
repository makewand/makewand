package serverusage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSQLiteMonthStartBoundaryIncludesSubSecond guards the fixed-width timestamp
// fix: an entry at the very start of the month with sub-second precision must be
// included by a `>= month-start` filter (string comparison used to drop it).
func TestSQLiteMonthStartBoundaryIncludesSubSecond(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	monthStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Log(Entry{Timestamp: monthStart.Add(123 * time.Millisecond), OrganizationID: "org1", CostUSD: 5}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	entries, err := store.Load(CurrentMonthFilter(Filter{OrgID: "org1"}, monthStart.Add(time.Hour)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("month-start sub-second entry was dropped: got %d entries, want 1", len(entries))
	}
	if entries[0].CostUSD != 5 {
		t.Fatalf("cost = %v, want 5", entries[0].CostUSD)
	}
}

// TestSQLiteUntilBoundaryIncludesEqualInstant guards against the regression where
// an inclusive `<= Until` query dropped a stored entry at the exact same
// sub-second instant (a fixed-width boundary vs a variable-width stored row).
func TestSQLiteUntilBoundaryIncludesEqualInstant(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	ts := time.Date(2026, 8, 1, 0, 0, 0, 123000000, time.UTC) // …00:00:00.123Z
	if err := store.Log(Entry{Timestamp: ts, OrganizationID: "org1", CostUSD: 1}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	entries, err := store.Load(Filter{OrgID: "org1", Until: ts})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inclusive Until at the equal instant dropped the entry: got %d, want 1", len(entries))
	}
}

func TestSQLiteLoadNilReceiverErrors(t *testing.T) {
	var s *SQLiteStore
	if _, err := s.Load(Filter{}); err == nil {
		t.Fatal("nil SQLiteStore.Load should return an error (unavailable), not empty results")
	}
}

func TestJSONLReaderLoadNilReceiverErrors(t *testing.T) {
	var r *JSONLReader
	if _, err := r.Load(Filter{}); err == nil {
		t.Fatal("nil JSONLReader.Load should return an error (unavailable)")
	}
}
