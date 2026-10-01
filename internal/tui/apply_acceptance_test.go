package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
)

func TestFileApplicationRejectsUnissuedAcceptanceAndPreservesWorkspace(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")
	project, err := engine.NewProject("acceptance-apply", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFile("a.txt", "before"); err != nil {
		t.Fatal(err)
	}
	app.project = project
	app.pendingFiles = []engine.ExtractedFile{{Path: "a.txt", Content: "after"}}
	app.pendingWriteAcceptance = &engine.TrustedAcceptanceRecord{Schema: 1, Driver: "forged", Isolated: true, CasesPassed: 1}
	model, command := app.handleFileWriteConfirm(confirmFileWriteMsg{confirmed: true})
	if model.(App).pendingWriteAcceptance != nil {
		t.Fatal("pending acceptance was retained after consuming approval")
	}
	result := command().(fileWriteCompleteMsg)
	if result.written != 0 || result.failed != 1 || !strings.Contains(strings.Join(result.errors, " "), "acceptance") {
		t.Fatalf("forged record accepted: %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(project.Path, "a.txt"))
	if err != nil || string(content) != "before" {
		t.Fatalf("changed workspace: %q %v", content, err)
	}
}

func TestFileApplicationBatchPreparationFailureWritesNothing(t *testing.T) {
	app := *NewApp(ModeChat, config.DefaultConfig(), "")
	project, err := engine.NewProject("atomic-apply", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFile("a.txt", "before"); err != nil {
		t.Fatal(err)
	}
	app.project = project
	app.pendingFiles = []engine.ExtractedFile{{Path: "a.txt", Content: "after"}, {Path: ".git/config", Content: "forbidden"}}
	_, command := app.handleFileWriteConfirm(confirmFileWriteMsg{confirmed: true})
	result := command().(fileWriteCompleteMsg)
	if result.written != 0 || result.failed != 2 {
		t.Fatalf("partial batch reported successful: %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(project.Path, "a.txt"))
	if err != nil || string(content) != "before" {
		t.Fatalf("partial batch applied: %q %v", content, err)
	}
}
