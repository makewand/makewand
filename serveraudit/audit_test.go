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
