package serveraudit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJSONLLogger_WritesEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	defer logger.Close()

	logger.Log(Event{
		Timestamp: time.Date(2026, 3, 23, 0, 0, 0, 0, time.UTC),
		Kind:      "chat",
		TokenID:   "runner",
		Status:    200,
	})

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open audit log: %v", err)
	}
	defer f.Close()

	var evt Event
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&evt); err != nil {
		t.Fatalf("Decode audit event: %v", err)
	}
	if evt.Kind != "chat" {
		t.Fatalf("Kind = %q, want %q", evt.Kind, "chat")
	}
	if evt.TokenID != "runner" {
		t.Fatalf("TokenID = %q, want %q", evt.TokenID, "runner")
	}
	if evt.Status != 200 {
		t.Fatalf("Status = %d, want 200", evt.Status)
	}
}

func TestLoadEventsAndSummarize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	logger.Log(Event{
		Timestamp:      time.Date(2026, 3, 23, 0, 0, 0, 0, time.UTC),
		Kind:           "chat",
		TokenID:        "runner",
		Status:         200,
		ActualProvider: "codex",
	})
	logger.Log(Event{
		Timestamp:   time.Date(2026, 3, 23, 1, 0, 0, 0, time.UTC),
		Kind:        "session",
		TokenID:     "viewer",
		Status:      403,
		WorkspaceID: "repo-main",
	})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events, err := LoadEvents(path, Filter{TokenID: "runner"})
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	summary := SummarizeEvents(events)
	if summary.Total != 1 {
		t.Fatalf("summary.Total = %d, want 1", summary.Total)
	}
	if summary.ByProvider["codex"] != 1 {
		t.Fatalf("summary.ByProvider[codex] = %d, want 1", summary.ByProvider["codex"])
	}

	allEvents, err := LoadEvents(path, Filter{})
	if err != nil {
		t.Fatalf("LoadEvents(all): %v", err)
	}
	if len(allEvents) != 2 {
		t.Fatalf("len(allEvents) = %d, want 2", len(allEvents))
	}
}
