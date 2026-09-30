package serverusage

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSQLiteAggregateAndPagination(t *testing.T) {
	s, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Timestamp: now, RequestID: "a", OrganizationID: "org", ProjectID: "project", TokenID: "token", ActualProvider: "mock", CostUSD: 2, PromptTokens: 3},
		{Timestamp: now.Add(time.Nanosecond), RequestID: "b", OrganizationID: "org", ProjectID: "project", TokenID: "token", ActualProvider: "mock", CostUSD: -1, CompletionTokens: 4},
		{Timestamp: now.Add(time.Second), RequestID: "c", OrganizationID: "org", ProjectID: "project", CostUSD: 3},
	}
	for _, entry := range entries {
		if err := s.Log(entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, filter := range []Filter{{}, {OrgID: "org"}, {ProjectID: "project", Limit: 1, Offset: 1}, {Since: now, Until: now.Add(time.Nanosecond)}} {
		loaded, err := s.Load(filter)
		if err != nil {
			t.Fatal(err)
		}
		summary, err := s.Summarize(filter)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(summary, SummarizeEntries(loaded)) {
			t.Fatalf("SQL aggregate differs from legacy summary: %#v %#v", summary, SummarizeEntries(loaded))
		}
	}
	spent, count, err := s.Spend(Filter{OrgID: "org"})
	if err != nil {
		t.Fatal(err)
	}
	if spent != 5 || count != 3 {
		t.Fatalf("spend/count=%v/%d", spent, count)
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT * FROM usage_entries WHERE organization_id=? AND timestamp>=? ORDER BY timestamp DESC LIMIT 100`, "org", now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if contains(detail, "usage_org_time") {
			indexed = true
		}
	}
	if !indexed {
		t.Fatal("organization query did not use time index")
	}
}

func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
