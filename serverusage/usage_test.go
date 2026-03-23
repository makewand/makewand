package serverusage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJSONLAndSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	logger, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	logger.Log(Entry{
		Timestamp:        time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC),
		RequestID:        "req_1",
		TokenID:          "runner",
		ActualProvider:   "codex",
		Status:           200,
		PromptTokens:     10,
		CompletionTokens: 5,
		CostUSD:          0.5,
	})
	logger.Log(Entry{
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
