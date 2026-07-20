package serverusage

import (
	"path/filepath"
	"testing"
)

// TestSQLiteLogReturnsErrorAfterClose guards that a failed INSERT is reported,
// not swallowed — a dropped write permanently under-counts budget.
func TestSQLiteLogReturnsErrorAfterClose(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	if err := store.Log(Entry{RequestID: "ok"}); err != nil {
		t.Fatalf("Log before close should succeed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Log(Entry{RequestID: "after-close"}); err == nil {
		t.Fatal("Log after Close should return an error, got nil")
	}
}

// TestJSONLLogReturnsErrorAfterClose does the same for the JSONL logger.
func TestJSONLLogReturnsErrorAfterClose(t *testing.T) {
	logger, err := OpenJSONL(filepath.Join(t.TempDir(), "usage.jsonl"))
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	if err := logger.Log(Entry{RequestID: "ok"}); err != nil {
		t.Fatalf("Log before close should succeed: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After Close the sink is unavailable; Log must return an error (not a silent
	// no-op) so strict accounting cannot treat an unrecorded entry as success.
	if err := logger.Log(Entry{RequestID: "after-close"}); err == nil {
		t.Fatal("Log after Close should return an error (sink unavailable)")
	}
}
