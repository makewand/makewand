package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

func TestBuildSystemPrompt_ExplainOmitsProjectTree(t *testing.T) {
	proj := &engine.Project{
		Name: "demo",
		Files: []engine.FileEntry{
			{Path: "."},
			{Path: "main.go"},
			{Path: "pkg/service.go"},
		},
	}

	prompt := buildSystemPrompt(proj, model.TaskExplain)
	if strings.Contains(prompt, "Project files:") {
		t.Fatalf("explain prompt should omit full project tree, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Project entries: 2") {
		t.Fatalf("explain prompt should include project entry count, got:\n%s", prompt)
	}
}

func TestBuildSystemPrompt_CodeUsesCompactProjectTree(t *testing.T) {
	files := []engine.FileEntry{{Path: "."}}
	for i := 0; i < 500; i++ {
		files = append(files, engine.FileEntry{
			Path: fmt.Sprintf("src/module/file_%03d.go", i),
		})
	}

	proj := &engine.Project{
		Name:  "huge",
		Files: files,
	}

	prompt := buildSystemPrompt(proj, model.TaskCode)
	if !strings.Contains(prompt, "Project files:") {
		t.Fatalf("code prompt should include project files section, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "more files not shown") {
		t.Fatalf("code prompt should truncate oversized file trees, got:\n%s", prompt)
	}
	if len(prompt) > 13000 {
		t.Fatalf("code prompt too large (%d bytes), want bounded prompt", len(prompt))
	}
}
