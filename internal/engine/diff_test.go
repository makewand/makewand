package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEdits_Basic(t *testing.T) {
	input := `Here's the fix:

--- EDIT: src/app.js ---
<<<< SEARCH
console.log("old");
const x = 1;
====
console.log("new");
const x = 2;
>>>> REPLACE

That should work!`

	edits := ParseEdits(input)
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}
	if edits[0].Path != "src/app.js" {
		t.Errorf("path = %q, want %q", edits[0].Path, "src/app.js")
	}
	if !strings.Contains(edits[0].Search, `console.log("old")`) {
		t.Errorf("search missing expected text: %q", edits[0].Search)
	}
	if !strings.Contains(edits[0].Replace, `console.log("new")`) {
		t.Errorf("replace missing expected text: %q", edits[0].Replace)
	}
}

func TestParseEdits_Multiple(t *testing.T) {
	input := `--- EDIT: file1.go ---
<<<< SEARCH
old1
====
new1
>>>> REPLACE

--- EDIT: file2.go ---
<<<< SEARCH
old2
====
new2
>>>> REPLACE
`

	edits := ParseEdits(input)
	if len(edits) != 2 {
		t.Fatalf("expected 2 edits, got %d", len(edits))
	}
	if edits[0].Path != "file1.go" {
		t.Errorf("edit 0 path = %q, want %q", edits[0].Path, "file1.go")
	}
	if edits[1].Path != "file2.go" {
		t.Errorf("edit 1 path = %q, want %q", edits[1].Path, "file2.go")
	}
}

func TestParseEdits_MultiplePairsUnderOneHeader(t *testing.T) {
	input := `--- EDIT: main.go ---
<<<< SEARCH
func old1() {}
====
func new1() {}
>>>> REPLACE

<<<< SEARCH
func old2() {}
====
func new2() {}
>>>> REPLACE
`

	edits := ParseEdits(input)
	if len(edits) != 2 {
		t.Fatalf("expected 2 edits, got %d", len(edits))
	}
	if edits[0].Path != "main.go" || edits[1].Path != "main.go" {
		t.Errorf("both edits should target main.go, got %q and %q", edits[0].Path, edits[1].Path)
	}
	if !strings.Contains(edits[0].Search, "old1") {
		t.Errorf("edit 0 search = %q", edits[0].Search)
	}
	if !strings.Contains(edits[1].Search, "old2") {
		t.Errorf("edit 1 search = %q", edits[1].Search)
	}
}

func TestParseEdits_Empty(t *testing.T) {
	edits := ParseEdits("Just some text with no edits.")
	if len(edits) != 0 {
		t.Errorf("expected 0 edits, got %d", len(edits))
	}
}

func TestParseDiffs_Basic(t *testing.T) {
	input := `Here's the patch:

--- DIFF: src/main.go ---
@@ -10,4 +10,5 @@
 func main() {
-    fmt.Println("old")
+    fmt.Println("new")
+    fmt.Println("extra")
 }

Done!`

	diffs := ParseDiffs(input)
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].Path != "src/main.go" {
		t.Errorf("path = %q, want %q", diffs[0].Path, "src/main.go")
	}
	if !strings.Contains(diffs[0].Patch, "@@ -10,4 +10,5 @@") {
		t.Errorf("patch missing hunk header: %q", diffs[0].Patch)
	}
}

func TestParseDiffs_Multiple(t *testing.T) {
	input := `--- DIFF: a.go ---
@@ -1,3 +1,3 @@
 line1
-old
+new
 line3
--- DIFF: b.go ---
@@ -1,2 +1,2 @@
-foo
+bar
 baz
`

	diffs := ParseDiffs(input)
	if len(diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %d", len(diffs))
	}
	if diffs[0].Path != "a.go" {
		t.Errorf("diff 0 path = %q", diffs[0].Path)
	}
	if diffs[1].Path != "b.go" {
		t.Errorf("diff 1 path = %q", diffs[1].Path)
	}
}

func TestParseDiffs_Empty(t *testing.T) {
	diffs := ParseDiffs("No diffs here.")
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs, got %d", len(diffs))
	}
}

func TestApplyEdit_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	original := "line1\nline2\nold code\nline4\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	edit := EditBlock{
		Path:    "test.txt",
		Search:  "old code",
		Replace: "new code",
	}
	if err := ApplyEdit(path, edit); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "new code") {
		t.Errorf("file should contain 'new code': %q", string(data))
	}
	if strings.Contains(string(data), "old code") {
		t.Errorf("file should not contain 'old code': %q", string(data))
	}
}

func TestApplyEdit_FirstOccurrenceOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	original := "AAA\nBBB\nAAA\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	edit := EditBlock{Search: "AAA", Replace: "CCC"}
	if err := ApplyEdit(path, edit); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "CCC") {
		t.Error("first occurrence not replaced")
	}
	if !strings.Contains(content, "AAA") {
		t.Error("second occurrence should remain")
	}
}

func TestApplyEdit_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}

	edit := EditBlock{Search: "nonexistent text", Replace: "x"}
	err := ApplyEdit(path, edit)
	if err == nil {
		t.Fatal("expected error for missing search text")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

func TestApplyEdit_FileNotFound(t *testing.T) {
	edit := EditBlock{Search: "x", Replace: "y"}
	err := ApplyEdit("/nonexistent/path/file.txt", edit)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestApplyDiff_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	original := "line1\nline2\nold line\nline4\nline5\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	diff := DiffBlock{
		Path: "test.txt",
		Patch: `@@ -2,3 +2,4 @@
 line2
-old line
+new line
+extra line
 line4`,
	}
	if err := ApplyDiff(path, diff); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "new line") {
		t.Errorf("should contain 'new line': %q", content)
	}
	if !strings.Contains(content, "extra line") {
		t.Errorf("should contain 'extra line': %q", content)
	}
	if strings.Contains(content, "old line") {
		t.Errorf("should not contain 'old line': %q", content)
	}
}

func TestApplyDiff_ContextMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	original := "line1\nline2\nline3\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	diff := DiffBlock{
		Path: "test.txt",
		Patch: `@@ -1,3 +1,3 @@
 line1
-wrong context
+new
 line3`,
	}
	err := ApplyDiff(path, diff)
	if err == nil {
		t.Fatal("expected error for context mismatch")
	}
	if !strings.Contains(err.Error(), "context mismatch") {
		t.Errorf("error should mention 'context mismatch': %v", err)
	}
}

func TestApplyDiff_MalformedHunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	diff := DiffBlock{
		Path:  "test.txt",
		Patch: "no hunk headers here, just garbage",
	}
	err := ApplyDiff(path, diff)
	if err == nil {
		t.Fatal("expected error for malformed diff")
	}
	if !strings.Contains(err.Error(), "no valid hunks") {
		t.Errorf("error should mention 'no valid hunks': %v", err)
	}
}

func TestApplyDiff_FileNotFound(t *testing.T) {
	diff := DiffBlock{Path: "x.txt", Patch: "@@ -1,1 +1,1 @@\n-a\n+b\n"}
	err := ApplyDiff("/nonexistent/path/file.txt", diff)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestContainsEdits(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"edit block", "--- EDIT: foo.js ---\n<<<< SEARCH\nold\n====\nnew\n>>>> REPLACE", true},
		{"diff block", "--- DIFF: bar.go ---\n@@ -1,1 +1,1 @@\n-old\n+new", true},
		{"no edits", "just some text", false},
		{"file block not edit", "--- FILE: foo.txt ---\n```\nhello\n```", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainsEdits(tt.input)
			if got != tt.want {
				t.Errorf("ContainsEdits() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseFilesWithEditsAndDiffs(t *testing.T) {
	input := `Here are the changes:

--- EDIT: src/app.js ---
<<<< SEARCH
old
====
new
>>>> REPLACE

--- DIFF: src/main.go ---
@@ -1,2 +1,2 @@
-old
+new
 ctx

--- FILE: new_file.txt ---
` + "```" + `
brand new content
` + "```" + `

Done!`

	result := ParseFiles(input)

	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(result.Files))
	}
	if result.Files[0].Path != "new_file.txt" {
		t.Errorf("file path = %q, want new_file.txt", result.Files[0].Path)
	}
	if len(result.Edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(result.Edits))
	}
	if result.Edits[0].Path != "src/app.js" {
		t.Errorf("edit path = %q, want src/app.js", result.Edits[0].Path)
	}
	if len(result.Diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(result.Diffs))
	}
	if result.Diffs[0].Path != "src/main.go" {
		t.Errorf("diff path = %q, want src/main.go", result.Diffs[0].Path)
	}

	// Explanation should not contain the EDIT/DIFF blocks
	if strings.Contains(result.Explanation, "<<<< SEARCH") {
		t.Error("explanation should not contain EDIT block markers")
	}
	if strings.Contains(result.Explanation, "@@ -1,2") {
		t.Error("explanation should not contain DIFF hunk headers")
	}
}
