package serverteam

import (
	"bytes"
	"encoding/csv"
	"testing"
)

// Organization and project names are chosen by members, so a name such as
// =HYPERLINK(...) must reach a spreadsheet as text, while numeric columns keep
// their type.
func TestTeamCSVExportsKeepFormulaNamesAsText(t *testing.T) {
	bucket := BillingBucket{ID: "org_1", Name: "=HYPERLINK(\"http://x\")", MonthlyBudgetUSD: 10, SpendUSD: -0.5, RequestCount: 3}
	var summary bytes.Buffer
	if err := WriteBillingSummaryCSV(&summary, BillingSummary{Organizations: []BillingBucket{bucket}}); err != nil {
		t.Fatal(err)
	}
	var alerts bytes.Buffer
	alert := BudgetAlert{ScopeType: "project", ID: "+cmd", Name: "@SUM(A1)", Severity: "-warning", RequestCount: 1}
	if err := WriteBudgetAlertsCSV(&alerts, []BudgetAlert{alert}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		data []byte
		want map[int]string
	}{
		"billing summary": {summary.Bytes(), map[int]string{2: `'=HYPERLINK("http://x")`, 4: "-0.500000", 7: "3"}},
		"budget alerts":   {alerts.Bytes(), map[int]string{1: "'+cmd", 2: "'@SUM(A1)", 3: "'-warning", 8: "1"}},
	} {
		records, err := csv.NewReader(bytes.NewReader(tc.data)).ReadAll()
		if err != nil || len(records) != 2 {
			t.Fatalf("%s: records=%v err=%v", name, records, err)
		}
		for column, want := range tc.want {
			if got := records[1][column]; got != want {
				t.Errorf("%s column %d = %q, want %q", name, column, got, want)
			}
		}
	}
}
