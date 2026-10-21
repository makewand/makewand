package router

import "testing"

// The price table is spelled in lower case while a configured model may not
// be (OPENAI_MODEL=" O1 "): a miss on case or padding billed o1 at the
// fallback rate, a fifth of its price, and gpt-4o-mini at twenty times its.
func TestCostForMatchesModelNamesWithoutCaseOrPadding(t *testing.T) {
	tables := baseTables.copy()
	for _, id := range []string{"o1", "O1", " O1 ", "GPT-4o-Mini", "gpt-4o-mini"} {
		if _, ok := tables.costFor(id); !ok {
			t.Errorf("costFor(%q) found no price", id)
		}
	}
	if got, want := mustCost(t, tables, " O1 "), mustCost(t, tables, "o1"); got != want {
		t.Errorf("costFor(\" O1 \") = %+v, want o1's %+v", got, want)
	}
	if _, ok := tables.costFor("o1-unknown"); ok {
		t.Error("an unknown model must stay unpriced")
	}
}

func mustCost(t *testing.T, tables *strategyTables, id string) costEntry {
	t.Helper()
	e, ok := tables.costFor(id)
	if !ok {
		t.Fatalf("no price for %q", id)
	}
	return e
}
