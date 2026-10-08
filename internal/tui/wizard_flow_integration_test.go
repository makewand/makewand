package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

func TestWizardBuildsProjectFromTemplateWithoutPTY(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DefaultModel = "private"
	cfg.AnalysisModel = "private"
	cfg.CodingModel = "private"
	cfg.ReviewModel = "private"

	provider := &wizardRetryStubProvider{
		name: "private",
		responses: []string{
			"Plan:\n- Build a simple page\n- No dependencies\n- No tests\n",
			"--- FILE: index.html ---\n```\n<html><body><h1>hello</h1></body></html>\n```",
			"LGTM",
		},
	}

	app := *NewApp(ModeNew, cfg, "")
	app.router = newWizardFlowRouter(t, cfg, provider)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	runDir := t.TempDir()
	if err := os.Chdir(runDir); err != nil {
		t.Fatalf("Chdir(%s): %v", runDir, err)
	}
	defer func() {
		_ = os.Chdir(origWD)
	}()

	enter := tea.KeyMsg{Type: tea.KeyEnter}

	var cmd tea.Cmd
	app, cmd = updateWizardApp(t, app, enter)
	if app.wizard.Phase() != WizardPhasePlan {
		t.Fatalf("phase after template enter = %v, want %v", app.wizard.Phase(), WizardPhasePlan)
	}
	app = runWizardCmds(t, app, cmd)

	app, cmd = updateWizardApp(t, app, enter)
	if cmd != nil {
		t.Fatal("plan confirmation should not return a command")
	}
	if app.wizard.Phase() != WizardPhaseConfirm {
		t.Fatalf("phase after plan confirm = %v, want %v", app.wizard.Phase(), WizardPhaseConfirm)
	}

	app, cmd = updateWizardApp(t, app, enter)
	if app.project == nil {
		t.Fatal("project should be created during build confirmation")
	}
	app = runWizardCmds(t, app, cmd)

	projectFile := filepath.Join(runDir, "blog-project", "index.html")
	data, err := os.ReadFile(projectFile)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", projectFile, err)
	}
	if !strings.Contains(string(data), "<h1>hello</h1>") {
		t.Fatalf("generated file content = %q, want hello heading", string(data))
	}
	if app.wizard.Phase() != WizardPhaseDone {
		t.Fatalf("final wizard phase = %v, want %v", app.wizard.Phase(), WizardPhaseDone)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (plan, implementation, review)", provider.calls)
	}
}

func newWizardFlowRouter(t *testing.T, cfg *config.Config, provider model.Provider) *model.Router {
	t.Helper()

	r, err := model.NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := r.RegisterProvider("private", provider, model.AccessSubscription); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	return r
}

func updateWizardApp(t *testing.T, app App, msg tea.Msg) (App, tea.Cmd) {
	t.Helper()

	modelValue, cmd := app.Update(msg)
	next, ok := modelValue.(App)
	if !ok {
		t.Fatalf("Update() returned %T, want App", modelValue)
	}
	return next, cmd
}

func runWizardCmds(t *testing.T, app App, cmd tea.Cmd) App {
	t.Helper()

	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		nextCmd := queue[0]
		queue = queue[1:]
		if nextCmd == nil {
			continue
		}

		msg := nextCmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}

		var followUp tea.Cmd
		app, followUp = updateWizardApp(t, app, msg)
		queue = append(queue, followUp)
	}

	return app
}

func TestDisambiguateProjectName(t *testing.T) {
	parent := t.TempDir()

	// 1. Available when dir does not exist
	if got := disambiguateProjectName(parent, "app"); got != "app" {
		t.Errorf("disambiguate(non-existent) = %q, want app", got)
	}

	// 2. Available when dir exists and is empty
	emptyDir := filepath.Join(parent, "empty-proj")
	if err := os.Mkdir(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := disambiguateProjectName(parent, "empty-proj"); got != "empty-proj" {
		t.Errorf("disambiguate(empty) = %q, want empty-proj", got)
	}

	// 3. Disambiguates when dir exists and is not empty
	nonEmpty := filepath.Join(parent, "app")
	if err := os.Mkdir(nonEmpty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "file.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := disambiguateProjectName(parent, "app"); got != "app-2" {
		t.Errorf("disambiguate(non-empty) = %q, want app-2", got)
	}

	// 4. Disambiguates to -3 when -2 is also occupied
	nonEmpty2 := filepath.Join(parent, "app-2")
	if err := os.Mkdir(nonEmpty2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty2, "file.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := disambiguateProjectName(parent, "app"); got != "app-3" {
		t.Errorf("disambiguate(non-empty app & app-2) = %q, want app-3", got)
	}
}

func TestWizardBuild_AvoidsCollidingProjectDirectory(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DefaultModel = "private"
	cfg.AnalysisModel = "private"
	cfg.CodingModel = "private"
	cfg.ReviewModel = "private"

	provider := &wizardRetryStubProvider{
		name: "private",
		responses: []string{
			"Plan:\n- Build a simple page\n",
			"--- FILE: index.html ---\n```\n<html><body><h1>new version</h1></body></html>\n```",
			"LGTM",
		},
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	runDir := t.TempDir()
	if err := os.Chdir(runDir); err != nil {
		t.Fatalf("Chdir(%s): %v", runDir, err)
	}
	defer func() {
		_ = os.Chdir(origWD)
	}()

	// Pre-create blog-project with precious user files
	existingBlogDir := filepath.Join(runDir, "blog-project")
	if err := os.Mkdir(existingBlogDir, 0o700); err != nil {
		t.Fatal(err)
	}
	preciousFile := filepath.Join(existingBlogDir, "index.html")
	if err := os.WriteFile(preciousFile, []byte("precious original content"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := *NewApp(ModeNew, cfg, "")
	app.router = newWizardFlowRouter(t, cfg, provider)

	enter := tea.KeyMsg{Type: tea.KeyEnter}

	var cmd tea.Cmd
	// Template select -> Plan
	app, cmd = updateWizardApp(t, app, enter)
	app = runWizardCmds(t, app, cmd)

	// Plan -> Confirm
	app, cmd = updateWizardApp(t, app, enter)

	// Confirm -> Build
	app, cmd = updateWizardApp(t, app, enter)
	if app.project == nil {
		t.Fatal("project should be created during build confirmation")
	}
	if app.project.Name != "blog-project-2" {
		t.Fatalf("project name = %q, want disambiguated %q", app.project.Name, "blog-project-2")
	}

	app = runWizardCmds(t, app, cmd)

	// Verify original blog-project was completely untouched
	originalContent, err := os.ReadFile(preciousFile)
	if err != nil {
		t.Fatalf("ReadFile original: %v", err)
	}
	if string(originalContent) != "precious original content" {
		t.Fatalf("original project file was overwritten! got %q", string(originalContent))
	}

	// Verify blog-project-2 received the new generated file
	newFile := filepath.Join(runDir, "blog-project-2", "index.html")
	newContent, err := os.ReadFile(newFile)
	if err != nil {
		t.Fatalf("ReadFile new: %v", err)
	}
	if !strings.Contains(string(newContent), "new version") {
		t.Fatalf("new project file content = %q, want new version", string(newContent))
	}
}

func TestBuildPhase_RequiresApprovalWhenFilesAlreadyExist(t *testing.T) {
	app := newBuildAppForDepsTest(t)
	// Create an existing file in the project
	if err := app.project.WriteFile("existing.html", "user-content"); err != nil {
		t.Fatal(err)
	}

	// Attempt to extract/write the existing file during pendingPhaseBuild in manual mode
	modelAfterFiles, cmd := app.handleFilesExtracted(filesExtractedMsg{
		files: []engine.ExtractedFile{{Path: "existing.html", Content: "overwritten-content"}},
		phase: pendingPhaseBuild,
	})
	app = modelAfterFiles.(App)

	if cmd != nil {
		t.Fatal("pendingPhaseBuild must NOT auto-confirm file write when target file already exists")
	}
	if app.state != StateConfirmFiles {
		t.Fatalf("state = %v, want StateConfirmFiles", app.state)
	}
}
