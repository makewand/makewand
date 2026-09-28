package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// go-engine-tui-cmd#6: an oversized file the candidate did not touch used to
// make the whole diff (and so every candidate) fail with "file too large".
func TestDiffAgainst_LargeFilesDoNotFailCandidate(t *testing.T) {
	project, err := NewProject("large", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	large := make([]byte, maxReadFileSize+1<<20)
	for i := range large {
		large[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(project.Path, "assets.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project.Path, "main.py"), "print(1)\n")
	writeFile(t, filepath.Join(project.Path, "model.bin"), "small")
	if err := project.ScanFiles(); err != nil {
		t.Fatal(err)
	}
	clone, err := project.CloneToTemp()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(clone.Path)
	writeFile(t, filepath.Join(clone.Path, "main.py"), "print(2)\n")

	files, deleted, err := clone.ChangedFilesAgainstWithDeletions(project)
	if err != nil {
		t.Fatalf("ChangedFilesAgainstWithDeletions failed on an untouched large file: %v", err)
	}
	if len(files) != 1 || files[0].Path != "main.py" || len(deleted) != 0 {
		t.Fatalf("files=%v deleted=%v, want only main.py", files, deleted)
	}
	diff, err := clone.DiffAgainst(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.LargeUnchanged) != 1 || diff.LargeUnchanged[0] != "assets.bin" || len(diff.LargeChanged) != 0 {
		t.Fatalf("large file not recorded as skipped/unchanged: %+v", diff)
	}

	// A candidate that grows a file past the limit (or rewrites a large one) is
	// reported as not applicable instead of failing the candidate.
	large[0] ^= 0xff
	if err := os.WriteFile(filepath.Join(clone.Path, "assets.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone.Path, "model.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	diff, err = clone.DiffAgainst(project)
	if err != nil {
		t.Fatalf("DiffAgainst with changed large files: %v", err)
	}
	if strings.Join(diff.LargeChanged, ",") != "assets.bin,model.bin" {
		t.Fatalf("LargeChanged = %v, want [assets.bin model.bin]", diff.LargeChanged)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "main.py" {
		t.Fatalf("Files = %v, want only main.py", diff.Files)
	}
}

// go-engine-tui-cmd#7: edits to baseline test files are discarded from the
// delivered set; the selection must carry them so the UI can say so.
func TestCandidateSelectionReportsDiscardedTestEdits(t *testing.T) {
	passed := CandidateAttempt{Verification: CandidateVerification{Passed: true, Strength: 1, RestoredTests: []string{"math_test.go"}}}
	if got := discardedTestEdits(passed); len(got) != 1 || got[0] != "math_test.go" {
		t.Fatalf("discardedTestEdits(passed) = %v, want [math_test.go]", got)
	}
	// Unverified candidates go to manual approval with their raw content,
	// test edits included: nothing was discarded.
	failed := CandidateAttempt{Verification: CandidateVerification{RestoredTests: []string{"math_test.go"}, TestsError: "boom"}}
	if got := discardedTestEdits(failed); len(got) != 0 {
		t.Fatalf("discardedTestEdits(failed) = %v, want none", got)
	}
}

func TestEvaluateCandidateFiles_RestoredTestsExcludedFromVerifiedFiles(t *testing.T) {
	project := newVerificationProject(t)
	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{
		{Path: "math.go", Content: "package verify\n\nfunc Add(a, b int) int { return b + a }\n"},
		{Path: "math_test.go", Content: "package verify\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {}\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || strings.Join(report.RestoredTests, ",") != "math_test.go" {
		t.Fatalf("report = passed %v restored %v", report.Passed, report.RestoredTests)
	}
	for _, f := range report.VerifiedFiles {
		if f.Path == "math_test.go" {
			t.Fatal("discarded test edit is part of the delivered files")
		}
		if strings.Contains(f.Path, "makewand_canary") {
			t.Fatalf("verification canary leaked into the delivered files: %s", f.Path)
		}
	}
}

// go-engine-tui-cmd#10: Python suites declared outside pytest.ini were not
// detected, so candidates were reported as passing without running tests.
func TestDetectTestPlan_PythonSuites(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		detector string
		command  string
	}{
		{"pytest.ini", map[string]string{"pytest.ini": "[pytest]\n"}, "pytest.ini", "pytest"},
		{"pyproject ini_options", map[string]string{"pyproject.toml": "[project]\nname='x'\n\n[tool.pytest.ini_options]\naddopts='-q'\n"}, "pyproject.toml", "pytest"},
		{"pyproject tool.pytest", map[string]string{"pyproject.toml": "[tool.pytest]\n"}, "pyproject.toml", "pytest"},
		{"setup.cfg", map[string]string{"setup.cfg": "[metadata]\nname=x\n[tool:pytest]\ntestpaths=tests\n"}, "setup.cfg", "pytest"},
		{"tox.ini", map[string]string{"tox.ini": "[tox]\n[pytest]\n"}, "tox.ini", "pytest"},
		{"conftest", map[string]string{"conftest.py": "", "app.py": ""}, "conftest.py", "pytest"},
		{"tests dir", map[string]string{"app.py": "", "tests/test_app.py": "def test_x(): pass\n"}, "tests/test_app.py", "pytest"},
		{"suffix style", map[string]string{"pkg/app_test.py": ""}, "pkg/app_test.py", "pytest"},
		{"pyproject without pytest", map[string]string{"pyproject.toml": "[project]\nname='x'\n", "app.py": ""}, "", ""},
		// Declared runners keep priority over convention-only Python files.
		{"go module with helper script", map[string]string{"go.mod": "module x\n", "scripts/test_helper.py": ""}, "go.mod", "go"},
		{"venv tests ignored", map[string]string{"app.py": "", ".venv/lib/test_x.py": ""}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project, err := NewProject("py", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for rel, content := range tc.files {
				writeFile(t, filepath.Join(project.Path, filepath.FromSlash(rel)), content)
			}
			plan, err := project.DetectTestPlan()
			if err != nil {
				t.Fatal(err)
			}
			if tc.detector == "" {
				if plan != nil {
					t.Fatalf("plan = %+v, want none", plan)
				}
				return
			}
			if plan == nil || plan.Detector != tc.detector || plan.Command != tc.command {
				t.Fatalf("plan = %+v, want %s via %s", plan, tc.command, tc.detector)
			}
		})
	}
}

func TestEvaluateCandidateFiles_NoTestPlanIsNotPassed(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	allowHostExecForTest(t)
	project, err := NewProject("notests", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{Acknowledged: true, Source: "test"})
	writeFile(t, filepath.Join(project.Path, "app.py"), "def add(a, b):\n    return a + b\n")
	if err := project.ScanFiles(); err != nil {
		t.Fatal(err)
	}
	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "app.py", Content: "def add(a, b):\n    return a - b\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Strength != 0 {
		t.Fatalf("candidate without any test plan reported Passed=%v Strength=%d", report.Passed, report.Strength)
	}
	if !report.NoTestPlan || !report.NoTestsRan {
		t.Fatalf("report must say no tests were executed: %+v", report)
	}
	attempt := CandidateAttempt{Provider: "p", Content: "x", Verification: report}
	if ShouldRecordCandidateQuality(attempt) {
		t.Fatal("an unverifiable (no test plan) candidate must not move provider quality statistics")
	}
	if stage := CandidateAttemptStage(context.Background(), attempt); stage != CandidateProgressUnverified {
		t.Fatalf("progress stage = %s, want unverified", stage)
	}
}

func TestAttemptOutranksPrefersUnverifiedOverFailedChecks(t *testing.T) {
	unverified := CandidateAttempt{Index: 1, Verification: CandidateVerification{NoTestPlan: true}}
	broken := CandidateAttempt{Index: 0, Verification: CandidateVerification{QuickCheckError: "syntax error"}}
	if !attemptOutranks(unverified, broken) {
		t.Fatal("a candidate that failed its checks outranked one that could not be verified")
	}
	passed := CandidateAttempt{Index: 2, Verification: CandidateVerification{Passed: true, Strength: 1}}
	if !attemptOutranks(passed, unverified) {
		t.Fatal("a passing candidate must outrank an unverified one")
	}
}

// go-engine-tui-cmd#10: the per-command budget was a hard 5 minutes, shorter
// than the 10-minute context the TUI hands to dependency installs and tests.
func TestRestrictedExecContextHonorsCallerBudget(t *testing.T) {
	if RestrictedPlanTimeout < 10*time.Minute {
		t.Fatalf("RestrictedPlanTimeout = %s, shorter than the TUI budget", RestrictedPlanTimeout)
	}
	parent, cancel := context.WithTimeout(context.Background(), RestrictedPlanTimeout)
	defer cancel()
	ctx, cancelExec := restrictedExecContext(parent)
	defer cancelExec()
	parentDeadline, _ := parent.Deadline()
	deadline, ok := ctx.Deadline()
	if !ok || parentDeadline.Sub(deadline) > time.Second {
		t.Fatalf("exec deadline %v cuts the caller's %v budget short", deadline, parentDeadline)
	}
	unbounded, cancelUnbounded := restrictedExecContext(context.Background())
	defer cancelUnbounded()
	if d, ok := unbounded.Deadline(); !ok || time.Until(d) > RestrictedPlanTimeout {
		t.Fatalf("commands without a caller deadline must still be bounded: %v %v", d, ok)
	}
}

// go-engine-tui-cmd#12: rolling back a failed batch left the directory chain
// WriteFiles had created for a new file.
func TestCheckpointRestoreRemovesCreatedDirectories(t *testing.T) {
	project, err := NewProject("rollback", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project.Path, "existing", "keep.go"), "package existing\n")
	files := []ExtractedFile{
		{Path: "new/deep/dir/file.go", Content: "package dir\n"},
		{Path: "existing/sub/new.go", Content: "package sub\n"},
	}
	checkpoint, err := project.CheckpointFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFiles(files); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Restore(); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"new", "existing/sub"} {
		if _, err := os.Lstat(filepath.Join(project.Path, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Fatalf("directory %s created by the rolled-back write survived: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(project.Path, "existing", "keep.go")); err != nil {
		t.Fatalf("pre-existing content removed by rollback: %v", err)
	}
}

func TestCheckpointRestoreKeepsDirectoriesThatGainedOtherFiles(t *testing.T) {
	project, err := NewProject("rollback-shared", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := []ExtractedFile{{Path: "gen/a.go", Content: "package gen\n"}}
	checkpoint, err := project.CheckpointFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteFiles(files); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project.Path, "gen", "user.txt"), "added meanwhile")
	if err := checkpoint.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project.Path, "gen", "user.txt")); err != nil {
		t.Fatalf("rollback removed a file it did not create: %v", err)
	}
}
