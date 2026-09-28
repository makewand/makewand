package router

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestLoadUserOverridesReplacesPreviousOverrides pins the (hot-reload driven)
// contract behind go-router#5: each LoadUserOverrides call re-applies
// routing.json over the built-in defaults, so the most recently loaded file is
// the only override in effect; a missing file leaves the tables unchanged.
func TestLoadUserOverridesReplacesPreviousOverrides(t *testing.T) {
	r := mustNewRouter(RouterConfig{})
	defaultClaude := r.ContextBudgetForProvider("claude", TierMid)
	defaultCodex := r.ContextBudgetForProvider("codex", TierMid)

	dirA, dirB, empty := t.TempDir(), t.TempDir(), t.TempDir()
	writeRoutingJSON(t, dirA, `{"context_budgets":{"claude":{"mid":12345}}}`)
	writeRoutingJSON(t, dirB, `{"context_budgets":{"codex":{"mid":999}}}`)

	if err := r.LoadUserOverrides(dirA); err != nil {
		t.Fatal(err)
	}
	if got := r.ContextBudgetForProvider("claude", TierMid); got != 12345 {
		t.Fatalf("after A: claude/mid = %d, want 12345", got)
	}
	if err := r.LoadUserOverrides(dirB); err != nil {
		t.Fatal(err)
	}
	if got := r.ContextBudgetForProvider("claude", TierMid); got != defaultClaude {
		t.Fatalf("after B: claude/mid = %d, want the built-in default %d (B replaces A)", got, defaultClaude)
	}
	if got := r.ContextBudgetForProvider("codex", TierMid); got != 999 || got == defaultCodex {
		t.Fatalf("after B: codex/mid = %d, want 999", got)
	}
	if err := r.LoadUserOverrides(empty); err != nil {
		t.Fatal(err)
	}
	if got := r.ContextBudgetForProvider("codex", TierMid); got != 999 {
		t.Fatalf("missing routing.json changed the tables: codex/mid = %d, want 999", got)
	}
}

// TestLoadUserOverridesDocMatchesReplaceSemantics is the doc regression for
// go-router#5: the exported doc comments promised a deep-merge "over the
// current snapshot", which the implementation (deliberately) never did.
func TestLoadUserOverridesDocMatchesReplaceSemantics(t *testing.T) {
	fset := token.NewFileSet()
	docs := map[string]string{}
	for _, file := range []string{"router.go", "strategy_tables.go", "reload.go"} {
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok {
						name = id.Name + "." + name
					}
				}
			}
			docs[name] = fn.Doc.Text()
		}
	}
	for _, name := range []string{"Router.LoadUserOverrides", "strategyTables.loadUserOverrides", "Router.WatchOverrides", "LoadUserOverrides"} {
		doc, ok := docs[name]
		if !ok {
			t.Fatalf("no doc comment found for %s", name)
		}
		flat := strings.Join(strings.Fields(doc), " ")
		if strings.Contains(flat, "current snapshot; absent fields keep their values") ||
			strings.Contains(flat, "deep-merges configDir/routing.json into this Router's tables") ||
			strings.Contains(flat, "deep-merged into this Router's strategy tables") {
			t.Fatalf("%s doc still promises merging over the current tables:\n%s", name, doc)
		}
		if !strings.Contains(flat, "defaults") {
			t.Fatalf("%s doc does not say overrides apply over the built-in defaults:\n%s", name, doc)
		}
	}
}
