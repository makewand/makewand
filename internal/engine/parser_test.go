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
