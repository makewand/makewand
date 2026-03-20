package engine

import (
	"context"
	"os"
	"testing"
)

func newVerificationProject(t *testing.T) *Project {
	t.Helper()

	project, err := NewProject("verify-project", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	files := []ExtractedFile{
		{
			Path: "go.mod",
			Content: `module example.com/verify

go 1.22
`,
		},
		{
			Path: "math.go",
			Content: `package verify

func Add(a, b int) int {
	return a + b
}
`,
		},
		{
			Path: "math_test.go",
			Content: `package verify

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 2); got != 4 {
		t.Fatalf("Add(2,2) = %d, want 4", got)
	}
}
`,
		},
	}
	if err := project.WriteFiles(files); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}
	return project
}

func TestEvaluateCandidateFiles_PassingCandidate(t *testing.T) {
	project := newVerificationProject(t)

	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path: "math.go",
		Content: `package verify

func Add(a, b int) int {
	return a + b
}
`,
	}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed {
		t.Fatalf("report.Passed = false, want true (tests error: %q)", report.TestsError)
	}
	if !report.HasTests {
		t.Fatal("report.HasTests = false, want true")
	}
	if report.Strength != 2 {
		t.Fatalf("report.Strength = %d, want 2", report.Strength)
	}
	if !report.DepsSkipped {
		t.Fatal("report.DepsSkipped = false, want true for unchanged Go module metadata")
	}
}

func TestEvaluateCandidateFiles_FailingCandidate(t *testing.T) {
	project := newVerificationProject(t)

	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path: "math.go",
		Content: `package verify

func Add(a, b int) int {
	return a - b
}
`,
	}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false")
	}
	if report.TestsError == "" {
		t.Fatal("report.TestsError = empty, want test failure details")
	}
	if report.QuickCheckError != "" {
		t.Fatalf("report.QuickCheckError = %q, want empty for logic failure", report.QuickCheckError)
	}
}

func TestEvaluateCandidateFiles_SyntaxErrorFailsFast(t *testing.T) {
	project := newVerificationProject(t)

	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path: "math.go",
		Content: `package verify

func Add(a, b int) int {
	return a +
}
`,
	}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false")
	}
	if report.QuickCheckError == "" {
		t.Fatal("report.QuickCheckError = empty, want fast syntax failure")
	}
	if report.TestsPlan != nil {
		t.Fatalf("report.TestsPlan = %+v, want nil after fast failure", report.TestsPlan)
	}
	if report.DepsPlan != nil {
		t.Fatalf("report.DepsPlan = %+v, want nil after fast failure", report.DepsPlan)
	}
}

func TestEvaluateCandidateFiles_GoModChangeKeepsDepsVerification(t *testing.T) {
	project := newVerificationProject(t)

	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path: "go.mod",
		Content: `module example.com/verify

go 1.22
`,
	}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.DepsPlan == nil {
		t.Fatal("report.DepsPlan = nil, want go mod tidy plan")
	}
	if report.DepsSkipped {
		t.Fatal("report.DepsSkipped = true, want false when go.mod changed")
	}
}

func TestFileCheckpoint_Restore(t *testing.T) {
	project := newVerificationProject(t)
	files := []ExtractedFile{
		{Path: "math.go", Content: "package verify\n\nfunc Add(a, b int) int { return 0 }\n"},
		{Path: "new.txt", Content: "temp"},
	}

	checkpoint, err := project.CheckpointFiles(files)
	if err != nil {
		t.Fatalf("CheckpointFiles: %v", err)
	}
	if err := project.WriteFiles(files); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := checkpoint.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	content, err := project.ReadFile("math.go")
	if err != nil {
		t.Fatalf("ReadFile(math.go): %v", err)
	}
	if content == "package verify\n\nfunc Add(a, b int) int { return 0 }\n" {
		t.Fatal("math.go was not restored")
	}
	if _, err := project.ReadFile("new.txt"); err == nil {
		t.Fatal("new.txt should have been removed by restore")
	}
}

func TestChangedFilesAgainst_ReturnsAddedAndModifiedFiles(t *testing.T) {
	project := newVerificationProject(t)
	clone, err := project.CloneToTemp()
	if err != nil {
		t.Fatalf("CloneToTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(clone.Path) }()

	if err := clone.WriteFiles([]ExtractedFile{
		{
			Path: "math.go",
			Content: `package verify

func Add(a, b int) int {
	return a - b
}
`,
		},
		{
			Path:    "extra.txt",
			Content: "hello",
		},
	}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	files, err := clone.ChangedFilesAgainst(project)
	if err != nil {
		t.Fatalf("ChangedFilesAgainst: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Path != "extra.txt" || files[1].Path != "math.go" {
		t.Fatalf("paths = (%s, %s), want (extra.txt, math.go)", files[0].Path, files[1].Path)
	}
}
