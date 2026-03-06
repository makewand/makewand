package engine

import (
	"reflect"
	"strings"
	"testing"
)

func FuzzParseFiles(f *testing.F) {
	seeds := []string{
		"",
		"Just some text with no files.",
		"--- FILE: index.html ---\n```\n<h1>Hello</h1>\n```",
		"```js src/app.js\nconsole.log('hi')\n```",
		"**style.css**\n```\nbody {}\n```",
		"### main.go\n```\npackage main\n```",
		"File: README.md\n```\n# Title\n```",
		"```html\nindex.html\n```",
		"```python app.py\nprint('hi')\n```",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		strict := ParseFiles(input)
		if again := ParseFiles(input); !reflect.DeepEqual(strict, again) {
			t.Fatalf("ParseFiles(%q) is not deterministic", input)
		}

		bestEffort := ParseFilesBestEffort(input)
		if again := ParseFilesBestEffort(input); !reflect.DeepEqual(bestEffort, again) {
			t.Fatalf("ParseFilesBestEffort(%q) is not deterministic", input)
		}

		if len(strict.Files) > 0 && !reflect.DeepEqual(strict, bestEffort) {
			t.Fatalf("ParseFilesBestEffort(%q) should preserve strict parse result", input)
		}

		for _, result := range []ParseResult{strict, bestEffort} {
			for _, file := range result.Files {
				if strings.TrimSpace(file.Path) == "" {
					t.Fatalf("parser returned empty file path for input %q", input)
				}
			}
		}
	})
}
