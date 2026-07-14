package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRepoContext_Rules(t *testing.T) {
	dir := t.TempDir()
	rulesDir := filepath.Join(dir, ".makewand")
	if err := os.MkdirAll(rulesDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rulesDir, "rules.md"), []byte("Always use tabs.\nNo globals."), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rc, err := LoadRepoContext(dir, nil)
	if err != nil {
		t.Fatalf("LoadRepoContext: %v", err)
	}
	if rc.Rules != "Always use tabs.\nNo globals." {
		t.Errorf("Rules = %q, want %q", rc.Rules, "Always use tabs.\nNo globals.")
	}
}

func TestLoadRepoContext_MissingRules(t *testing.T) {
	dir := t.TempDir()
	rc, err := LoadRepoContext(dir, nil)
	if err != nil {
		t.Fatalf("LoadRepoContext: %v", err)
	}
	if rc.Rules != "" {
		t.Errorf("Rules should be empty, got %q", rc.Rules)
	}
}

func TestLoadRepoContext_FileHints(t *testing.T) {
	dir := t.TempDir()
	content := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files := []FileEntry{{Path: "main.go", IsDir: false}}
	rc, err := LoadRepoContext(dir, files)
	if err != nil {
		t.Fatalf("LoadRepoContext: %v", err)
	}
	hint, ok := rc.FileHints["main.go"]
	if !ok {
		t.Fatal("expected file hint for main.go")
	}
	// Should contain first 5 lines.
	lines := strings.Split(hint, "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 hint lines, got %d", len(lines))
	}
}

func TestExtractSymbols_Go(t *testing.T) {
	dir := t.TempDir()
	src := `package main

func main() {}

type Router struct {}

func (r *Router) Handle() {}

func helperFunc() {}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files := []FileEntry{{Path: "main.go"}}
	symbols := ExtractSymbols(dir, files)

	want := map[string]string{
		"main":       "func",
		"Router":     "type",
		"Handle":     "func",
		"helperFunc": "func",
	}
	got := make(map[string]string)
	for _, s := range symbols {
		got[s.Name] = s.Kind
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("symbol %s: got kind %q, want %q", name, got[name], kind)
		}
	}
}

func TestExtractSymbols_JS(t *testing.T) {
	dir := t.TempDir()
	src := `function handleRequest() {}
export default class App {}
export const VERSION = "1.0"
class Helper {}
`
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(src), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files := []FileEntry{{Path: "app.js"}}
	symbols := ExtractSymbols(dir, files)

	want := map[string]string{
		"handleRequest": "func",
		"App":           "class",
		"VERSION":       "const",
		"Helper":        "class",
	}
	got := make(map[string]string)
	for _, s := range symbols {
		got[s.Name] = s.Kind
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("symbol %s: got kind %q, want %q", name, got[name], kind)
		}
	}
}

func TestExtractSymbols_Python(t *testing.T) {
	dir := t.TempDir()
	src := `class MyApp:
    def run(self):
        pass

def main():
    app = MyApp()
`
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(src), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files := []FileEntry{{Path: "app.py"}}
	symbols := ExtractSymbols(dir, files)

	want := map[string]string{
		"MyApp": "class",
		"run":   "func",
		"main":  "func",
	}
	got := make(map[string]string)
	for _, s := range symbols {
		got[s.Name] = s.Kind
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("symbol %s: got kind %q, want %q", name, got[name], kind)
		}
	}
}

func TestForPrompt_Budget(t *testing.T) {
	rc := &RepoContext{
		Rules: strings.Repeat("Rule text. ", 100),
		Symbols: []Symbol{
			{Name: "main", Kind: "func", File: "main.go", Line: 1},
			{Name: "Router", Kind: "type", File: "router.go", Line: 5},
		},
		FileHints: map[string]string{
			"main.go": "package main",
		},
	}

	// Large budget: everything fits.
	full := rc.ForPrompt(10000)
	if !strings.Contains(full, "Project rules:") {
		t.Error("expected rules in full output")
	}
	if !strings.Contains(full, "Key symbols:") {
		t.Error("expected symbols in full output")
	}
	if !strings.Contains(full, "File hints:") {
		t.Error("expected file hints in full output")
	}

	// Tiny budget: should truncate gracefully and not exceed.
	tiny := rc.ForPrompt(100)
	if len(tiny) > 100 {
		t.Errorf("ForPrompt(100) returned %d chars, want <= 100", len(tiny))
	}
}

func TestForPrompt_Nil(t *testing.T) {
	var rc *RepoContext
	if got := rc.ForPrompt(1000); got != "" {
		t.Errorf("nil RepoContext.ForPrompt should return empty, got %q", got)
	}
}

func TestExtractSymbols_NonCodeFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# Hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files := []FileEntry{{Path: "readme.md"}}
	symbols := ExtractSymbols(dir, files)
	if len(symbols) != 0 {
		t.Errorf("expected no symbols for .md file, got %d", len(symbols))
	}
}

func TestExtractSymbols_SymbolLineNumbers(t *testing.T) {
	dir := t.TempDir()
	src := `package main

func first() {}

func second() {}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files := []FileEntry{{Path: "main.go"}}
	symbols := ExtractSymbols(dir, files)

	for _, s := range symbols {
		if s.Name == "first" && s.Line != 3 {
			t.Errorf("first: line %d, want 3", s.Line)
		}
		if s.Name == "second" && s.Line != 5 {
			t.Errorf("second: line %d, want 5", s.Line)
		}
	}
}
