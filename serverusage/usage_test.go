package serverusage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLAndSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	logger, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	_ = logger.Log(Entry{
		Timestamp:        time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC),
		RequestID:        "req_1",
		TokenID:          "runner",
		ActualProvider:   "codex",
		Status:           200,
		PromptTokens:     10,
		CompletionTokens: 5,
		CostUSD:          0.5,
	})
	_ = logger.Log(Entry{
		Timestamp:      time.Date(2026, 3, 23, 11, 0, 0, 0, time.UTC),
		RequestID:      "req_2",
		TokenID:        "viewer",
		ActualProvider: "codex",
		Status:         429,
		Stream:         true,
	})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("usage log missing: %v", err)
	}

	entries, err := LoadEntries(path, Filter{TokenID: "runner"})
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	summary := SummarizeEntries(entries)
	if summary.TotalRequests != 1 {
		t.Fatalf("TotalRequests = %d, want 1", summary.TotalRequests)
	}
	if summary.TotalCostUSD != 0.5 {
		t.Fatalf("TotalCostUSD = %.2f, want 0.50", summary.TotalCostUSD)
	}
}

func TestSQLiteStore_LogAndLoad(t *testing.T) {
	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	defer store.Close()

	_ = store.Log(Entry{
		Timestamp:      time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC),
		RequestID:      "req_1",
		TokenID:        "runner",
		ActualProvider: "codex",
		Status:         200,
		CostUSD:        0.25,
	})

	entries, err := store.Load(Filter{RequestID: "req_1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].RequestID != "req_1" {
		t.Fatalf("RequestID = %q, want req_1", entries[0].RequestID)
	}
}

func TestJSONLReaderAndMonthFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	logger, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	defer logger.Close()

	currentMonth := MonthStart(time.Now().UTC())
	previousMonth := currentMonth.AddDate(0, -1, 0)
	_ = logger.Log(Entry{Timestamp: previousMonth.Add(2 * time.Hour), RequestID: "old", CostUSD: 1})
	_ = logger.Log(Entry{Timestamp: currentMonth.Add(2 * time.Hour), RequestID: "new", CostUSD: 2})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewJSONLReader(path)
	entries, err := reader.Load(CurrentMonthFilter(Filter{}, time.Now().UTC()))
	if err != nil {
		t.Fatalf("Load(CurrentMonthFilter): %v", err)
	}
	if len(entries) != 1 || entries[0].RequestID != "new" {
		t.Fatalf("entries = %+v, want only current month record", entries)
	}
}

func TestWriteEntriesCSV(t *testing.T) {
	var buf bytes.Buffer
	err := WriteEntriesCSV(&buf, []Entry{{
		Timestamp:        time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC),
		RequestID:        "req_1",
		TokenID:          "runner",
		ActualProvider:   "codex",
		Status:           200,
		PromptTokens:     10,
		CompletionTokens: 5,
		CostUSD:          0.5,
	}})
	if err != nil {
		t.Fatalf("WriteEntriesCSV: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "request_id") || !strings.Contains(out, "req_1") {
		t.Fatalf("csv output missing expected values: %s", out)
	}
}
