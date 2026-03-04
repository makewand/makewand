package engine

import (
	"testing"
)

func TestParseFiles_FormatA(t *testing.T) {
	input := `Here's your blog project:

--- FILE: index.html ---
` + "```" + `
<!DOCTYPE html>
<html>
<head><title>Blog</title></head>
<body><h1>My Blog</h1></body>
</html>
` + "```" + `

--- FILE: style.css ---
` + "```" + `
body { margin: 0; font-family: sans-serif; }
h1 { color: #333; }
` + "```" + `

That's it!`

	result := ParseFiles(input)

	if len(result.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result.Files))
	}

	if result.Files[0].Path != "index.html" {
		t.Errorf("file 0 path = %q, want %q", result.Files[0].Path, "index.html")
	}
	if result.Files[1].Path != "style.css" {
		t.Errorf("file 1 path = %q, want %q", result.Files[1].Path, "style.css")
	}

	if !contains(result.Files[0].Content, "<h1>My Blog</h1>") {
		t.Errorf("file 0 content missing expected HTML: %q", result.Files[0].Content)
	}
	if !contains(result.Files[1].Content, "font-family") {
		t.Errorf("file 1 content missing expected CSS: %q", result.Files[1].Content)
	}

	if result.Explanation == "" {
		t.Error("expected non-empty explanation")
	}
}

func TestParseFiles_FormatB(t *testing.T) {
	input := "I'll create the files:\n\n" +
		"```python app.py\n" +
		"from flask import Flask\n" +
		"app = Flask(__name__)\n" +
		"```\n\n" +
		"```json package.json\n" +
		`{"name": "blog"}` + "\n" +
		"```\n"

	result := ParseFiles(input)

	if len(result.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result.Files))
	}

	if result.Files[0].Path != "app.py" {
		t.Errorf("file 0 path = %q, want %q", result.Files[0].Path, "app.py")
	}
	if result.Files[1].Path != "package.json" {
		t.Errorf("file 1 path = %q, want %q", result.Files[1].Path, "package.json")
	}
}

func TestParseFiles_Empty(t *testing.T) {
	result := ParseFiles("Just some text with no files.")
	if len(result.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(result.Files))
	}
}

func TestParseFiles_FormatC_Bold(t *testing.T) {
	input := "Here's the code:\n\n" +
		"**index.html**\n" +
		"```\n" +
		"<!DOCTYPE html>\n" +
		"<html><body>Hello</body></html>\n" +
		"```\n\n" +
		"**style.css**\n" +
		"```\n" +
		"body { margin: 0; }\n" +
		"```\n"

	result := ParseFiles(input)

	if len(result.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result.Files))
	}
	if result.Files[0].Path != "index.html" {
		t.Errorf("file 0 path = %q, want %q", result.Files[0].Path, "index.html")
	}
	if result.Files[1].Path != "style.css" {
		t.Errorf("file 1 path = %q, want %q", result.Files[1].Path, "style.css")
	}
}

func TestParseFiles_FormatC_BoldBacktick(t *testing.T) {
	input := "**`src/app.js`**\n```\nconsole.log('hi');\n```\n"

	result := ParseFiles(input)

	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(result.Files))
	}
	if result.Files[0].Path != "src/app.js" {
		t.Errorf("file 0 path = %q, want %q", result.Files[0].Path, "src/app.js")
	}
}

func TestParseFiles_FormatD_MdHeader(t *testing.T) {
	input := "### index.html\n```\n<h1>Hi</h1>\n```\n\n## style.css\n```\nh1 { color: red; }\n```\n"

	result := ParseFiles(input)

	if len(result.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result.Files))
	}
	if result.Files[0].Path != "index.html" {
		t.Errorf("file 0 path = %q, want %q", result.Files[0].Path, "index.html")
	}
	if result.Files[1].Path != "style.css" {
		t.Errorf("file 1 path = %q, want %q", result.Files[1].Path, "style.css")
	}
}

func TestParseFiles_FormatE_Colon(t *testing.T) {
	input := "File: main.py\n```\nprint('hello')\n```\n\nFile: `utils.py`\n```\ndef add(a, b): return a+b\n```\n"

	result := ParseFiles(input)

	if len(result.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result.Files))
	}
	if result.Files[0].Path != "main.py" {
		t.Errorf("file 0 path = %q, want %q", result.Files[0].Path, "main.py")
	}
	if result.Files[1].Path != "utils.py" {
		t.Errorf("file 1 path = %q, want %q", result.Files[1].Path, "utils.py")
	}
}

func TestContainsFiles(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"format A", "--- FILE: foo.txt ---\n```\nhello\n```", true},
		{"format B", "```js src/index.js\nconsole.log('hi')\n```", true},
		{"format C bold", "**index.html**\n```\nhello\n```", true},
		{"format C backtick", "**`src/app.js`**\n```\nhi\n```", true},
		{"format D header", "### index.html\n```\nhello\n```", true},
		{"format E colon", "File: main.py\n```\nprint('hi')\n```", true},
		{"plain text", "just some text", false},
		{"code block no file", "```\nsome code\n```", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainsFiles(tt.input)
			if got != tt.want {
				t.Errorf("ContainsFiles(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseFilesBestEffort_PathHintBeforeFence(t *testing.T) {
	input := "index.html\n" +
		"```html\n" +
		"<!doctype html><html><body>Hello</body></html>\n" +
		"```\n\n" +
		"style.css\n" +
		"```css\n" +
		"body { color: red; }\n" +
		"```\n"

	result := ParseFilesBestEffort(input)
	if len(result.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result.Files))
	}
	if result.Files[0].Path != "index.html" {
		t.Fatalf("file 0 path=%q, want index.html", result.Files[0].Path)
	}
	if result.Files[1].Path != "style.css" {
		t.Fatalf("file 1 path=%q, want style.css", result.Files[1].Path)
	}
}

func TestParseFilesBestEffort_LanguageOnlyMultipleFences(t *testing.T) {
	input := "Here are the files:\n\n" +
		"```html\n" +
		"<h1>Demo</h1>\n" +
		"```\n\n" +
		"```css\n" +
		"h1 { color: blue; }\n" +
		"```\n\n" +
		"```javascript\n" +
		"console.log('ok')\n" +
		"```\n"

	result := ParseFilesBestEffort(input)
	if len(result.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(result.Files))
	}
	if result.Files[0].Path != "index.html" || result.Files[1].Path != "style.css" || result.Files[2].Path != "script.js" {
		t.Fatalf("unexpected inferred paths: %+v", result.Files)
	}
}

func TestParseFilesBestEffort_InlinePathInFence(t *testing.T) {
	input := "```python src/app.py\n" +
		"print('hello')\n" +
		"```\n"

	result := ParseFilesBestEffort(input)
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(result.Files))
	}
	if result.Files[0].Path != "src/app.py" {
		t.Fatalf("file path=%q, want src/app.py", result.Files[0].Path)
	}
}

func TestParseFilesBestEffort_DoesNotInferSingleGenericFence(t *testing.T) {
	input := "Example snippet:\n```python\nprint('example only')\n```\n"
	result := ParseFilesBestEffort(input)
	if len(result.Files) != 0 {
		t.Fatalf("expected 0 files for single generic fence, got %d", len(result.Files))
	}
}

func TestParseFilesBestEffort_ParsesNestedFileBlocksInsideOuterFence(t *testing.T) {
	input := "```markdown\n" +
		"--- FILE: index.html ---\n" +
		"```html\n" +
		"<!doctype html><html><body>Nested</body></html>\n" +
		"```\n\n" +
		"--- FILE: style.css ---\n" +
		"```css\n" +
		"body { color: #222; }\n" +
		"```\n" +
		"```\n"

	result := ParseFilesBestEffort(input)
	if len(result.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result.Files))
	}
	if result.Files[0].Path != "index.html" {
		t.Fatalf("file 0 path=%q, want index.html", result.Files[0].Path)
	}
	if result.Files[1].Path != "style.css" {
		t.Fatalf("file 1 path=%q, want style.css", result.Files[1].Path)
	}
	if !contains(result.Files[0].Content, "Nested") {
		t.Fatalf("file 0 content not parsed from nested block: %q", result.Files[0].Content)
	}
}

func TestParseFilesBestEffort_UnclosedFenceStillExtractsFile(t *testing.T) {
	input := "```html\n" +
		"<!doctype html>\n" +
		"<html><body>partial</body></html>\n"

	result := ParseFilesBestEffort(input)
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(result.Files))
	}
	if result.Files[0].Path != "index.html" {
		t.Fatalf("file path=%q, want index.html", result.Files[0].Path)
	}
	if !contains(result.Files[0].Content, "partial") {
		t.Fatalf("file content should keep unclosed fence payload: %q", result.Files[0].Content)
	}
}

func TestParseFilesBestEffort_DoesNotInferShellCommandAsFile(t *testing.T) {
	input := "The app is ready:\n\n" +
		"```bash\n" +
		"open password-strength-checker/index.html\n" +
		"```\n\n" +
		"```text\n" +
		"password-strength-checker/\\nindex.html\\nstyle.css\\nscript.js\n" +
		"```\n"

	result := ParseFilesBestEffort(input)
	if len(result.Files) != 0 {
		t.Fatalf("expected 0 files for command-only response, got %d: %+v", len(result.Files), result.Files)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
