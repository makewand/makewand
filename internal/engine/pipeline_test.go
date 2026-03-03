package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestFullBuildPipeline simulates the complete build flow:
// AI response → parse files → write to disk → scan → install deps → run tests
func TestFullBuildPipeline(t *testing.T) {
	// Simulate AI response for a blog project
	aiResponse := `Great! Here's your blog project:

--- FILE: index.html ---
` + "```" + `
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>My Blog</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <header><h1>My Blog</h1></header>
  <main id="posts"></main>
  <script src="app.js"></script>
</body>
</html>
` + "```" + `

--- FILE: style.css ---
` + "```" + `
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: 'Segoe UI', sans-serif; background: #f5f5f5; }
header { background: #2d3436; color: white; padding: 2rem; text-align: center; }
main { max-width: 800px; margin: 2rem auto; padding: 0 1rem; }
.post { background: white; border-radius: 8px; padding: 1.5rem; margin-bottom: 1rem; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
.post h2 { color: #2d3436; margin-bottom: 0.5rem; }
.post p { color: #636e72; line-height: 1.6; }
.post .date { color: #b2bec3; font-size: 0.9rem; }
` + "```" + `

--- FILE: app.js ---
` + "```" + `
const posts = [
  { title: "Hello World", date: "2026-03-01", body: "Welcome to my blog!" },
  { title: "Second Post", date: "2026-03-02", body: "This is another post." }
];

function renderPosts() {
  const main = document.getElementById('posts');
  posts.forEach(post => {
    const div = document.createElement('div');
    div.className = 'post';
    div.innerHTML = '<h2>' + post.title + '</h2><p class="date">' + post.date + '</p><p>' + post.body + '</p>';
    main.appendChild(div);
  });
}

document.addEventListener('DOMContentLoaded', renderPosts);
` + "```" + `

--- FILE: package.json ---
` + "```" + `
{
  "name": "my-blog",
  "version": "1.0.0",
  "description": "A simple blog",
  "scripts": {
    "start": "python3 -m http.server 8080"
  }
}
` + "```" + `

Your blog is ready! Open index.html in a browser to see it.`

	// Step 1: Parse files from AI response
	t.Run("ParseFiles", func(t *testing.T) {
		result := ParseFiles(aiResponse)

		if len(result.Files) != 4 {
			t.Fatalf("expected 4 files, got %d", len(result.Files))
		}

		expectedFiles := map[string]bool{
			"index.html":   false,
			"style.css":    false,
			"app.js":       false,
			"package.json": false,
		}

		for _, f := range result.Files {
			if _, ok := expectedFiles[f.Path]; !ok {
				t.Errorf("unexpected file: %s", f.Path)
			}
			expectedFiles[f.Path] = true
			if f.Content == "" {
				t.Errorf("file %s has empty content", f.Path)
			}
		}

		for path, found := range expectedFiles {
			if !found {
				t.Errorf("expected file %s not found", path)
			}
		}

		if result.Explanation == "" {
			t.Error("expected non-empty explanation text")
		}
	})

	// Step 2: Create project and write files
	tmpDir := t.TempDir()

	proj, err := NewProject("test-blog", tmpDir)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	parsed := ParseFiles(aiResponse)

	t.Run("WriteFiles", func(t *testing.T) {
		var written int
		for _, f := range parsed.Files {
			if err := proj.WriteFile(f.Path, f.Content); err != nil {
				t.Errorf("WriteFile(%s): %v", f.Path, err)
			} else {
				written++
			}
		}

		if written != 4 {
			t.Errorf("wrote %d files, expected 4", written)
		}

		// Verify files exist on disk
		for _, f := range parsed.Files {
			fullPath := filepath.Join(proj.Path, f.Path)
			data, err := os.ReadFile(fullPath)
			if err != nil {
				t.Errorf("file %s not on disk: %v", f.Path, err)
				continue
			}
			if len(data) == 0 {
				t.Errorf("file %s is empty on disk", f.Path)
			}
		}
	})

	// Step 3: ScanFiles should find written files
	t.Run("ScanFiles", func(t *testing.T) {
		if err := proj.ScanFiles(); err != nil {
			t.Fatalf("ScanFiles: %v", err)
		}

		// Files list should include our 4 files (+ root dir ".")
		found := make(map[string]bool)
		for _, fe := range proj.Files {
			if fe.Path != "." {
				found[fe.Path] = true
			}
		}

		for _, f := range parsed.Files {
			if !found[f.Path] {
				t.Errorf("ScanFiles did not find %s", f.Path)
			}
		}
	})

	// Step 4: FileTree should produce output
	t.Run("FileTree", func(t *testing.T) {
		tree := proj.FileTree()
		if tree == "" {
			t.Error("FileTree returned empty string")
		}
		if !searchString(tree, "index.html") {
			t.Error("FileTree missing index.html")
		}
	})

	// Step 5: InstallDeps detects package.json
	t.Run("InstallDeps", func(t *testing.T) {
		ctx := context.Background()
		result, err := proj.InstallDeps(ctx)
		// npm install may fail (no npm or broken package.json in test) but
		// the key thing is it detected package.json and tried npm, not "No package manager"
		if err == nil && result != nil {
			if searchString(result.Stdout, "No package manager") {
				t.Error("InstallDeps should have detected package.json, not 'No package manager'")
			}
			t.Logf("InstallDeps result: exit=%d stdout=%q stderr=%q",
				result.ExitCode, truncate(result.Stdout, 200), truncate(result.Stderr, 200))
		} else if err != nil {
			t.Logf("InstallDeps error (expected in test env): %v", err)
		}
	})

	// Step 6: RunTests
	t.Run("RunTests", func(t *testing.T) {
		ctx := context.Background()
		result, err := proj.RunTests(ctx)
		if err != nil {
			t.Logf("RunTests error (expected in test env): %v", err)
		} else if result != nil {
			t.Logf("RunTests result: exit=%d stdout=%q",
				result.ExitCode, truncate(result.Stdout, 200))
		}
	})

	// Step 7: ReadFile round-trip
	t.Run("ReadFile", func(t *testing.T) {
		content, err := proj.ReadFile("index.html")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !searchString(content, "My Blog") {
			t.Error("ReadFile content missing 'My Blog'")
		}
	})

	// Step 8: Path traversal protection
	t.Run("PathTraversal", func(t *testing.T) {
		err := proj.WriteFile("../escape.txt", "bad")
		if err == nil {
			t.Error("expected error for path traversal, got nil")
		}
		err = proj.WriteFile("/etc/passwd", "bad")
		if err == nil {
			t.Error("expected error for absolute path, got nil")
		}
	})
}

// TestAutoFixParsing tests that a fix response with partial files is parsed correctly.
func TestAutoFixParsing(t *testing.T) {
	fixResponse := `I found the issue. The CSS file had a typo. Here's the fix:

--- FILE: style.css ---
` + "```" + `
body { margin: 0; font-family: sans-serif; }
` + "```" + `

Only the style.css file needed changing.`

	result := ParseFiles(fixResponse)

	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file in fix, got %d", len(result.Files))
	}
	if result.Files[0].Path != "style.css" {
		t.Errorf("fix file path = %q, want %q", result.Files[0].Path, "style.css")
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
