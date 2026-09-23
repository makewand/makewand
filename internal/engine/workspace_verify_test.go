package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// allowHostExecForTest opts verification into direct host execution via the
// documented escape hatch so unit tests do not depend on bubblewrap being
// installed. Sandbox wrapping itself is covered by verify_isolation_test.go.
func allowHostExecForTest(t *testing.T) {
	t.Helper()
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
}

func newVerificationProject(t *testing.T) *Project {
	t.Helper()
	allowHostExecForTest(t)

	project, err := NewProject("verify-project", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	// The env opt-in alone no longer authorizes host execution; tests carry the
	// acknowledged authorization the app layer would resolve.
	project.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{Acknowledged: true, Source: "test"})
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

func newTestlessVerificationProject(t *testing.T) *Project {
	t.Helper()
	allowHostExecForTest(t)

	project, err := NewProject("verify-notests", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	files := []ExtractedFile{
		{
			Path: "go.mod",
			Content: `module example.com/verifynotests

go 1.22
`,
		},
		{
			Path: "math.go",
			Content: `package verifynotests

func Add(a, b int) int {
	return a + b
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

func TestCheckpointFiles_LargeFilesPreservedOnRestore(t *testing.T) {
	project := newVerificationProject(t)
	// Create a file exceeding maxReadFileSize (10 MiB)
	largeName := "large.bin"
	fullPath := filepath.Join(project.Path, largeName)
	f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		t.Fatalf("Create large file: %v", err)
	}
	// Write 11 MiB of test pattern
	chunk := make([]byte, 1024*1024)
	for i := range chunk {
		chunk[i] = byte(i % 256)
	}
	hasher := sha256.New()
	for i := 0; i < 11; i++ {
		hasher.Write(chunk)
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			t.Fatalf("Write large chunk: %v", err)
		}
	}
	f.Close()
	expectedHash := hex.EncodeToString(hasher.Sum(nil))
	if err := os.Chmod(fullPath, 0755); err != nil {
		t.Fatalf("Chmod large file: %v", err)
	}

	files := []ExtractedFile{
		{
			Path:    largeName,
			Content: "small modified content",
		},
	}

	checkpoint, err := project.CheckpointFiles(files)
	if err != nil {
		t.Fatalf("CheckpointFiles on large file: %v", err)
	}

	// Overwrite large file with new content
	if err := project.WriteFiles(files); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	// Restore checkpoint
	if err := checkpoint.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Verify large file still exists, was restored to 11 MiB, AND preserved 0755 permissions!
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Fatalf("Large file was deleted or cannot be stated: %v", err)
	}
	if info.Size() != 11*1024*1024 {
		t.Fatalf("Large file size was corrupted: got %d, expected %d", info.Size(), 11*1024*1024)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("Large file permissions corrupted: got %v, expected 0755", info.Mode().Perm())
	}

	restoredFile, err := os.Open(fullPath)
	if err != nil {
		t.Fatalf("Open restored file: %v", err)
	}
	defer restoredFile.Close()
	restoreHasher := sha256.New()
	if _, err := io.Copy(restoreHasher, restoredFile); err != nil {
		t.Fatalf("Hash restored file: %v", err)
	}
	gotHash := hex.EncodeToString(restoreHasher.Sum(nil))
	if gotHash != expectedHash {
		t.Fatalf("Large file hash corrupted: got %s, expected %s", gotHash, expectedHash)
	}
}

func TestCheckpointFiles_RestoreFailurePreservesBackup(t *testing.T) {
	project := newVerificationProject(t)
	subDir := filepath.Join(project.Path, "sub")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	defer os.Chmod(subDir, 0755)

	largeName := "sub/large.bin"
	fullPath := filepath.Join(project.Path, largeName)
	f, err := os.Create(fullPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	chunk := make([]byte, 1024*1024)
	for i := 0; i < 11; i++ {
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			t.Fatalf("Write: %v", err)
		}
	}
	f.Close()

	checkpoint, err := project.CheckpointFiles([]ExtractedFile{{Path: largeName, Content: "mod"}})
	if err != nil {
		t.Fatalf("CheckpointFiles: %v", err)
	}
	bPaths := checkpoint.BackupPaths()
	if len(bPaths) == 0 {
		t.Fatalf("expected active backup path, got none")
	}
	bakFile := bPaths[0]

	// Make subDir read-only so Restore fails when creating temp restore file
	if err := os.Chmod(subDir, 0555); err != nil {
		t.Fatalf("Chmod subDir: %v", err)
	}

	restoreErr := checkpoint.Restore()
	if restoreErr == nil {
		t.Fatalf("expected Restore to fail in read-only dir, but succeeded")
	}

	// Verify backup file still exists on disk!
	if _, err := os.Stat(bakFile); err != nil {
		t.Fatalf("Backup file was deleted on restore failure: %v", err)
	}

	// Restore permissions and clean up
	os.Chmod(subDir, 0755)
	checkpoint.Cleanup()
	if _, err := os.Stat(bakFile); !os.IsNotExist(err) {
		t.Fatalf("expected backup to be removed after explicit Cleanup, got: %v", err)
	}
}

func TestCheckpointFiles_HardlinkConsistencyPreservedOnRestore(t *testing.T) {
	project := newVerificationProject(t)
	largeName := "large.bin"
	fullPath := filepath.Join(project.Path, largeName)
	linkName := "link.bin"
	linkPath := filepath.Join(project.Path, linkName)

	f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("Create large file: %v", err)
	}
	chunk := make([]byte, 1024*1024)
	for i := range chunk {
		chunk[i] = byte((i * 13) % 256)
	}
	hasher := sha256.New()
	for i := 0; i < 11; i++ {
		hasher.Write(chunk)
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			t.Fatalf("Write large chunk: %v", err)
		}
	}
	f.Close()
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	// Create hard link
	if err := os.Link(fullPath, linkPath); err != nil {
		t.Fatalf("Link %s -> %s: %v", fullPath, linkPath, err)
	}

	// Verify link count is 2 and inodes match
	infoOrig, err := os.Stat(fullPath)
	if err != nil {
		t.Fatalf("Stat %s: %v", fullPath, err)
	}
	infoLink, err := os.Stat(linkPath)
	if err != nil {
		t.Fatalf("Stat %s: %v", linkPath, err)
	}
	sysOrig := infoOrig.Sys().(*syscall.Stat_t)
	sysLink := infoLink.Sys().(*syscall.Stat_t)
	if sysOrig.Ino != sysLink.Ino {
		t.Fatalf("Inodes do not match: %d vs %d", sysOrig.Ino, sysLink.Ino)
	}
	if sysOrig.Nlink < 2 {
		t.Fatalf("Expected Nlink >= 2, got %d", sysOrig.Nlink)
	}

	// Checkpoint only large.bin
	checkpoint, err := project.CheckpointFiles([]ExtractedFile{
		{Path: largeName, Content: "mod"},
	})
	if err != nil {
		t.Fatalf("CheckpointFiles: %v", err)
	}

	// Overwrite large.bin with new content
	if err := project.WriteFiles([]ExtractedFile{
		{Path: largeName, Content: "modified content breaking link"},
	}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	// Restore checkpoint
	if err := checkpoint.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Verify large.bin has restored 11 MiB and matching hash
	fLarge, err := os.Open(fullPath)
	if err != nil {
		t.Fatalf("Open large: %v", err)
	}
	hLarge := sha256.New()
	if _, err := io.Copy(hLarge, fLarge); err != nil {
		fLarge.Close()
		t.Fatalf("Hash large: %v", err)
	}
	fLarge.Close()
	if hex.EncodeToString(hLarge.Sum(nil)) != expectedHash {
		t.Fatalf("large.bin hash mismatch after restore")
	}

	// Verify link.bin ALSO has restored 11 MiB, matching hash, and shares inode!
	fLink, err := os.Open(linkPath)
	if err != nil {
		t.Fatalf("Open link: %v", err)
	}
	hLink := sha256.New()
	if _, err := io.Copy(hLink, fLink); err != nil {
		fLink.Close()
		t.Fatalf("Hash link: %v", err)
	}
	fLink.Close()
	if hex.EncodeToString(hLink.Sum(nil)) != expectedHash {
		t.Fatalf("link.bin hash mismatch: hard link was not restored with large.bin!")
	}

	infoAfterOrig, _ := os.Stat(fullPath)
	infoAfterLink, _ := os.Stat(linkPath)
	sysAfterOrig := infoAfterOrig.Sys().(*syscall.Stat_t)
	sysAfterLink := infoAfterLink.Sys().(*syscall.Stat_t)
	if sysAfterOrig.Ino != sysAfterLink.Ino {
		t.Fatalf("Hard link broken! Inodes differ after restore: %d vs %d", sysAfterOrig.Ino, sysAfterLink.Ino)
	}
}

func TestCheckpointFiles_ParentDirectoryRecreatedOnRestore(t *testing.T) {
	project := newVerificationProject(t)
	subDir := filepath.Join(project.Path, "sub")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	largeName := "sub/large.bin"
	fullPath := filepath.Join(project.Path, largeName)
	f, err := os.Create(fullPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	chunk := make([]byte, 1024*1024)
	hasher := sha256.New()
	for i := 0; i < 11; i++ {
		hasher.Write(chunk)
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			t.Fatalf("Write large chunk: %v", err)
		}
	}
	f.Close()
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	checkpoint, err := project.CheckpointFiles([]ExtractedFile{
		{Path: largeName, Content: "mod"},
	})
	if err != nil {
		t.Fatalf("CheckpointFiles: %v", err)
	}

	// Delete the parent directory "sub" entirely
	if err := os.RemoveAll(subDir); err != nil {
		t.Fatalf("RemoveAll subDir: %v", err)
	}

	// Restore checkpoint - must recreate parent directory "sub" and restore large.bin!
	if err := checkpoint.Restore(); err != nil {
		t.Fatalf("Restore failed to recreate parent dir: %v", err)
	}

	// Verify large.bin exists and hash matches
	fRestored, err := os.Open(fullPath)
	if err != nil {
		t.Fatalf("Open restored file in recreated dir: %v", err)
	}
	defer fRestored.Close()
	hRestored := sha256.New()
	if _, err := io.Copy(hRestored, fRestored); err != nil {
		t.Fatalf("Hash restored: %v", err)
	}
	if hex.EncodeToString(hRestored.Sum(nil)) != expectedHash {
		t.Fatalf("Restored file hash mismatch")
	}
}

func TestEvaluateCandidateFiles_FailsClosedWithoutIsolation(t *testing.T) {
	project := newVerificationProject(t)
	fakeMissingBwrap(t) // overrides the env opt-in from the fixture helper

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
	if report.Passed {
		t.Fatal("report.Passed = true, want false when isolation is unavailable")
	}
	if report.Strength != 0 {
		t.Fatalf("report.Strength = %d, want 0", report.Strength)
	}
	if !strings.Contains(report.IsolationError, "bubblewrap") {
		t.Fatalf("report.IsolationError = %q, want bubblewrap reason", report.IsolationError)
	}
	if report.Isolated {
		t.Fatal("report.Isolated = true, want false")
	}
	if len(report.QuickCheckResults) != 0 || report.DepsResult != nil || report.TestsResult != nil {
		t.Fatalf("no command should execute without isolation; report=%+v", report)
	}
}

func TestEvaluateCandidateFiles_RestoresBaselineTests(t *testing.T) {
	project := newVerificationProject(t)

	// The candidate breaks Add and rewrites the baseline test so its own copy
	// would pass. Verification must judge it against the baseline test.
	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{
		{
			Path: "math.go",
			Content: `package verify

func Add(a, b int) int {
	return a - b
}
`,
		},
		{
			Path: "math_test.go",
			Content: `package verify

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 2); got != 0 {
		t.Fatalf("Add(2,2) = %d, want 0", got)
	}
}
`,
		},
	})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false against restored baseline tests")
	}
	if report.TestsError == "" {
		t.Fatal("report.TestsError = empty, want baseline test failure details")
	}
	if len(report.RestoredTests) != 1 || report.RestoredTests[0] != "math_test.go" {
		t.Fatalf("report.RestoredTests = %v, want [math_test.go]", report.RestoredTests)
	}
}

func TestEvaluateCandidateFiles_NoTestFilesCapsStrength(t *testing.T) {
	project := newTestlessVerificationProject(t)

	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path: "math.go",
		Content: `package verifynotests

func Add(a, b int) int {
	return b + a
}
`,
	}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed {
		t.Fatalf("report.Passed = false, want true (tests error: %q)", report.TestsError)
	}
	if report.Strength != 1 {
		t.Fatalf("report.Strength = %d, want 1 when no tests ran", report.Strength)
	}
	if !report.NoTestsRan {
		t.Fatal("report.NoTestsRan = false, want true")
	}
	if report.BaselineTests {
		t.Fatal("report.BaselineTests = true, want false for a module without test files")
	}
}

func TestEvaluateCandidateFiles_CandidateAddedTestsDoNotRaiseStrength(t *testing.T) {
	project := newTestlessVerificationProject(t)

	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{
		{
			Path: "math.go",
			Content: `package verifynotests

func Add(a, b int) int {
	return b + a
}
`,
		},
		{
			Path: "math_test.go",
			Content: `package verifynotests

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(1, 2); got != 3 {
		t.Fatalf("Add(1,2) = %d, want 3", got)
	}
}
`,
		},
	})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed {
		t.Fatalf("report.Passed = false, want true (tests error: %q)", report.TestsError)
	}
	if report.Strength != 1 {
		t.Fatalf("report.Strength = %d, want 1 for candidate-provided tests", report.Strength)
	}
}

func TestDetectNoTestsRun(t *testing.T) {
	tests := []struct {
		name   string
		plan   ExecPlan
		result *ExecResult
		want   bool
	}{
		{
			name:   "go with passing tests",
			plan:   ExecPlan{Command: "go"},
			result: &ExecResult{Stdout: "ok  \texample.com/verify\t0.002s\n"},
			want:   false,
		},
		{
			name:   "go without test files",
			plan:   ExecPlan{Command: "go"},
			result: &ExecResult{Stdout: "?   \texample.com/verify\t[no test files]\n"},
			want:   true,
		},
		{
			name:   "go with no tests to run",
			plan:   ExecPlan{Command: "go"},
			result: &ExecResult{Stdout: "ok  \texample.com/verify\t0.002s [no tests to run]\n"},
			want:   true,
		},
		{
			name:   "pytest no tests ran",
			plan:   ExecPlan{Command: "pytest"},
			result: &ExecResult{Stdout: "no tests ran in 0.01s\n"},
			want:   true,
		},
		{
			name:   "npm missing test script",
			plan:   ExecPlan{Command: "npm"},
			result: &ExecResult{Stderr: "Error: no test specified\n"},
			want:   true,
		},
		{
			name:   "npm node --test zero tests",
			plan:   ExecPlan{Command: "npm"},
			result: &ExecResult{Stdout: "# tests 0\n# pass 0\n# fail 0\n"},
			want:   true,
		},
		{
			name:   "npm node --test spec zero tests",
			plan:   ExecPlan{Command: "npm"},
			result: &ExecResult{Stdout: "ℹ tests 0\nℹ pass 0\n"},
			want:   true,
		},
		{
			name:   "npm zero tests phrase",
			plan:   ExecPlan{Command: "npm"},
			result: &ExecResult{Stdout: "found 0 tests\n"},
			want:   true,
		},
		{
			name:   "npm node --test ten tests",
			plan:   ExecPlan{Command: "npm"},
			result: &ExecResult{Stdout: "# tests 10\n# pass 10\n# fail 0\n"},
			want:   false,
		},
		{
			name:   "cargo zero tests",
			plan:   ExecPlan{Command: "cargo"},
			result: &ExecResult{Stdout: "running 0 tests\n"},
			want:   true,
		},
		{
			name:   "cargo with tests",
			plan:   ExecPlan{Command: "cargo"},
			result: &ExecResult{Stdout: "running 0 tests\nrunning 3 tests\n"},
			want:   false,
		},
		{
			name: "missing result",
			plan: ExecPlan{Command: "go"},
			want: true,
		},
	}

	for _, tt := range tests {
		if got := detectNoTestsRun(tt.plan, tt.result); got != tt.want {
			t.Fatalf("%s: detectNoTestsRun() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestMergeCandidateFiles_CloneDiffWins(t *testing.T) {
	reported := []ExtractedFile{
		{Path: "calc.go", Content: "reported"},
		{Path: "docs.md", Content: "docs"},
	}
	cloneDiff := []ExtractedFile{
		{Path: "calc.go", Content: "clone"},
		{Path: "extra.go", Content: "extra"},
	}

	merged := mergeCandidateFiles(reported, cloneDiff)
	if len(merged) != 3 {
		t.Fatalf("len(merged) = %d, want 3", len(merged))
	}
	byPath := make(map[string]string, len(merged))
	for _, f := range merged {
		byPath[f.Path] = f.Content
	}
	if byPath["calc.go"] != "clone" {
		t.Fatalf("calc.go content = %q, want clone diff to win", byPath["calc.go"])
	}
	if byPath["extra.go"] != "extra" || byPath["docs.md"] != "docs" {
		t.Fatalf("merged = %v, want union of both sets", byPath)
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

func TestChangedFilesAgainstWithDeletions_DetectsDeletedBaselineFiles(t *testing.T) {
	project := newVerificationProject(t)
	clone, err := project.CloneToTemp()
	if err != nil {
		t.Fatalf("CloneToTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(clone.Path) }()

	if err := os.Remove(filepath.Join(clone.Path, "math.go")); err != nil {
		t.Fatalf("Remove(math.go): %v", err)
	}
	if err := clone.WriteFiles([]ExtractedFile{{Path: "extra.txt", Content: "hello"}}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	files, deleted, err := clone.ChangedFilesAgainstWithDeletions(project)
	if err != nil {
		t.Fatalf("ChangedFilesAgainstWithDeletions: %v", err)
	}
	if len(files) != 1 || files[0].Path != "extra.txt" {
		t.Fatalf("files = %v, want only extra.txt", files)
	}
	if len(deleted) != 1 || deleted[0] != "math.go" {
		t.Fatalf("deleted = %v, want [math.go]", deleted)
	}
}
