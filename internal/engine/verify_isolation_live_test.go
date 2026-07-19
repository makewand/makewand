package engine

import (
	"context"
	"testing"
)

// TestEvaluateCandidateFiles_LiveBwrapIsolation exercises the real bubblewrap
// sandbox end-to-end. It skips when isolation is not available on the host so
// the suite never depends on bwrap being installed.
func TestEvaluateCandidateFiles_LiveBwrapIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live sandbox test in short mode")
	}
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	if !VerificationIsolationActive() {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError())
	}

	project, err := NewProject("verify-live", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := project.WriteFiles([]ExtractedFile{
		{Path: "go.mod", Content: "module example.com/verifylive\n\ngo 1.22\n"},
		{Path: "math.go", Content: "package verifylive\n\nfunc Add(a, b int) int { return a + b }\n"},
		{Path: "math_test.go", Content: "package verifylive\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"},
	}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}

	report, verifyErr := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path:    "math.go",
		Content: "package verifylive\n\nfunc Add(a, b int) int { return b + a }\n",
	}})
	if verifyErr != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", verifyErr)
	}
	if !report.Isolated {
		t.Fatalf("report.Isolated = false, want true; report=%+v", report)
	}
	if !report.Passed || report.Strength != 2 {
		t.Fatalf("report = %+v, want isolated pass at strength 2", report)
	}
}
