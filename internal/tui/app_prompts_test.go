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

	prompt := buildSystemPrompt(proj, model.TaskExplain, model.ModeBalanced)
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

	// ModeBalanced + TaskCode → claude mid → budget 10000 → scaled tree limits.
	// With 500 small files and a 10000-char budget the tree fits, so verify it's included.
	prompt := buildSystemPrompt(proj, model.TaskCode, model.ModeBalanced)
	if !strings.Contains(prompt, "Project files:") {
		t.Fatalf("code prompt should include project files section, got:\n%s", prompt)
	}
	// The scaled budget (10000) allows all 500 files; verify reasonable size.
	budget := model.ContextBudgetForMode(model.ModeBalanced, model.TaskCode)
	scale := float64(budget) / float64(model.DefaultContextBudget)
	maxExpected := int(float64(13000) * scale)
	if len(prompt) > maxExpected {
		t.Fatalf("code prompt too large (%d bytes), want bounded below %d", len(prompt), maxExpected)
	}
}

func TestBuildSystemPrompt_CodeTruncatesVeryLargeTree(t *testing.T) {
	// Use enough files to exceed even a scaled budget.
	files := []engine.FileEntry{{Path: "."}}
	for i := 0; i < 2000; i++ {
		files = append(files, engine.FileEntry{
			Path: fmt.Sprintf("src/module/file_%04d.go", i),
		})
	}

	proj := &engine.Project{
		Name:  "massive",
		Files: files,
	}

	prompt := buildSystemPrompt(proj, model.TaskCode, model.ModeFast)
	if !strings.Contains(prompt, "Project files:") {
		t.Fatalf("code prompt should include project files section, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "more files not shown") {
		t.Fatalf("code prompt should truncate oversized file trees")
	}
}
