package router

import (
	"os"
	"path/filepath"
	"testing"
)

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

// Overrides may spell a price key with capitals: it must replace the model's
// price, not add a second entry that wins only for that exact spelling, and a
// partial override must keep the other rate instead of zeroing it.
func TestCostOverridesUseTheNormalizedPriceKey(t *testing.T) {
	r := mustNewRouter(RouterConfig{})
	dir := t.TempDir()
	user := `{"costs": {"O1": {"input": 20, "output": 80}, "O3-Mini": {"input": 2}, "GPT-4.1-Mini": {"input": 0.4, "output": 1.6}}}`
	if err := os.WriteFile(filepath.Join(dir, "routing.json"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadUserOverrides(dir); err != nil {
		t.Fatal(err)
	}
	tables := r.routingTables()
	for _, id := range []string{"o1", "O1", " o1 "} {
		if got := mustCost(t, tables, id); got.Input != 20 || got.Output != 80 {
			t.Errorf("costFor(%q) = %+v, want the overridden 20/80", id, got)
		}
	}
	base := mustCost(t, baseTables.copy(), "o3-mini")
	if got := mustCost(t, tables, "o3-mini"); got.Input != 2 || got.Output != base.Output {
		t.Errorf("partial override of O3-Mini = %+v, want input 2 and the built-in output %v", got, base.Output)
	}
	for _, id := range []string{"gpt-4.1-mini", "GPT-4.1-MINI"} {
		if _, ok := tables.costFor(id); !ok {
			t.Errorf("costFor(%q) misses a model priced as GPT-4.1-Mini", id)
		}
	}
	for _, id := range tables.pricedModels() {
		if id != priceKey(id) {
			t.Errorf("price table keeps an unnormalized key %q", id)
		}
	}
}
