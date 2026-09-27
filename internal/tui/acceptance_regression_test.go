package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

func TestBuildReviewErrorAndUnresolvedDefectsBlockAcceptance(t *testing.T) {
	for _, review := range []codeReviewMsg{
		{err: errors.New("review service unavailable")},
		{content: "Severe defect: reject this implementation", hasIssues: true},
	} {
		app := newBuildAppWithMode(t, model.ModeBalanced)
		app.pipeline.SetPhase(engine.PhaseReview)
		updated, cmd := app.handleCodeReview(review)
		app = updated.(App)
		if cmd != nil || app.pipeline.Phase() != engine.PhaseBlocked || app.wizard.Phase() == WizardPhaseDone {
			t.Fatalf("review failure advanced build: phase=%v wizard=%v cmd=%v", app.pipeline.Phase(), app.wizard.Phase(), cmd != nil)
		}
		if app.progress.steps[stepReview].Status != StepFailed {
			t.Fatal("review failure hidden")
		}
	}
}

func TestSealedCandidatePayloadRetainsModeAndRejectsTampering(t *testing.T) {
	app := newBuildAppWithMode(t, model.ModeBalanced)
	app.project = newCandidateProject(t)
	path := filepath.Join(app.project.Path, "run.sh")
	//nolint:gosec // G306: this fixture must be executable to verify permission preservation or CLI availability.
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrue\n"), 0755); err != nil {
		t.Fatal(err)
	}
	report, err := app.project.EvaluateCandidateFiles(context.Background(), []engine.ExtractedFile{{Path: "run.sh", Content: "#!/bin/sh\necho checked\n"}})
	if err != nil || !report.Passed {
		t.Fatalf("verification: %v %+v", err, report)
	}
	updated, cmd := app.handleAIResponse(aiResponseMsg{content: report.VerifiedContent, files: report.VerifiedFiles, digest: report.VerifiedDigest})
	app = updated.(App)
	if cmd == nil {
		t.Fatal("missing file payload")
	}
	extracted := cmd().(filesExtractedMsg)
	if !extracted.files[0].ModeKnown || extracted.files[0].Mode != 0755 {
		t.Fatal("file metadata lost in UI")
	}
	app.pendingFiles = append([]engine.ExtractedFile(nil), extracted.files...)
	app.pendingFiles[0].Content = "unverified replacement"
	_, cmd = app.handleFileWriteConfirm(confirmFileWriteMsg{confirmed: true})
	result := cmd().(fileWriteCompleteMsg)
	if result.failed != 1 || result.written != 0 {
		t.Fatalf("tampered apply result: %+v", result)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "#!/bin/sh\ntrue\n" {
		t.Fatal("tampered payload written")
	}
}
