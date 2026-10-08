package router

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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

func TestLoadUserOverrides_DiscoveredAndUserPrecedence(t *testing.T) {
	r := mustNewRouter(RouterConfig{})
	dir := t.TempDir()

	// 1. Write discovered.json with dynamically discovered models
	disc := `{
		"models": {
			"claude": {"mid": "claude-discovered"},
			"codex": {"mid": "codex-discovered"}
		},
		"costs": {
			"claude-discovered": {"input": 3.0, "output": 15.0},
			"codex-discovered": {"input": 0.0, "output": 0.0}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "discovered.json"), []byte(disc), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.LoadUserOverrides(dir); err != nil {
		t.Fatalf("LoadUserOverrides with discovered.json failed: %v", err)
	}
	if got := r.routingTables().modelID("claude", TierMid); got != "claude-discovered" {
		t.Errorf("claude/mid = %q, want claude-discovered", got)
	}
	if got := r.routingTables().modelID("codex", TierMid); got != "codex-discovered" {
		t.Errorf("codex/mid = %q, want codex-discovered", got)
	}
	// Explicit zero pricing for codex-discovered preserved
	rate, ok := r.routingTables().costFor("codex-discovered")
	if !ok || rate.Input != 0 || rate.Output != 0 {
		t.Errorf("codex-discovered rate = %+v (ok=%v), want explicit 0", rate, ok)
	}

	// 2. Now user writes routing.json with pinned claude model. routing.json must override discovered.json!
	user := `{
		"models": {
			"claude": {"mid": "claude-pinned-by-user"}
		},
		"costs": {
			"claude-pinned-by-user": {"input": 4.0, "output": 20.0}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "routing.json"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.LoadUserOverrides(dir); err != nil {
		t.Fatalf("LoadUserOverrides with routing.json failed: %v", err)
	}
	// claude overridden by user's routing.json
	if got := r.routingTables().modelID("claude", TierMid); got != "claude-pinned-by-user" {
		t.Errorf("claude/mid = %q, want user pinned %q", got, "claude-pinned-by-user")
	}
	// codex still has discovered model from discovered.json
	if got := r.routingTables().modelID("codex", TierMid); got != "codex-discovered" {
		t.Errorf("codex/mid = %q, want discovered %q", got, "codex-discovered")
	}

	// 3. Invalid routing.json fails closed and leaves current tables intact
	if err := os.WriteFile(filepath.Join(dir, "routing.json"), []byte(`{invalid json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadUserOverrides(dir); err == nil {
		t.Fatal("LoadUserOverrides with malformed routing.json should return error, got nil")
	}
	// Tables must remain intact after failed reload
	if got := r.routingTables().modelID("claude", TierMid); got != "claude-pinned-by-user" {
		t.Errorf("claude/mid mutated after failed reload: got %q", got)
	}
}

func TestLoadUserOverrides_CLISubscriptionDiscoveredWithoutCostTableEntry(t *testing.T) {
	r := mustNewRouter(RouterConfig{})
	dir := t.TempDir()

	// discovered.json with CLI subscription models (codex, agy, muse, grok, local) and no cost entries
	disc := `{
		"models": {
			"codex": {"cheap": "gpt-6-luna", "mid": "gpt-6.1-sol", "premium": "gpt-6-astra"},
			"agy": {"cheap": "gemini-3.8-flash-high", "mid": "gemini-3.8-flash-high", "premium": "gemini-3.1-pro-high"},
			"muse": {"cheap": "muse-spark-1.2", "mid": "muse-spark-1.3-contributor", "premium": "muse-spark-1.3"},
			"grok": {"cheap": "grok-4.7-build-fast", "mid": "grok-4.7", "premium": "grok-4.7"},
			"local": {"cheap": "qwen2.5-coder:7b", "mid": "qwen2.5-coder:7b", "premium": "qwen2.5-coder:7b"}
		},
		"costs": {}
	}`
	if err := os.WriteFile(filepath.Join(dir, "discovered.json"), []byte(disc), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.LoadUserOverrides(dir); err != nil {
		t.Fatalf("LoadUserOverrides with CLI subscription models failed: %v", err)
	}

	if got := r.routingTables().modelID("codex", TierPremium); got != "gpt-6-astra" {
		t.Errorf("codex/premium = %q, want gpt-6-astra", got)
	}
	if got := r.routingTables().modelID("local", TierMid); got != "qwen2.5-coder:7b" {
		t.Errorf("local/mid = %q, want qwen2.5-coder:7b", got)
	}
	// Must not have injected 0 cost entries into cost table
	if _, ok := r.routingTables().costFor("gpt-6-astra"); ok {
		t.Errorf("gpt-6-astra should not have an injected cost table entry")
	}
}
