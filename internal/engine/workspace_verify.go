package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type fileCheckpointEntry struct {
	Path    string
	Existed bool
	Content string
}

// FileCheckpoint stores the pre-change state for a set of project files.
type FileCheckpoint struct {
	project *Project
	entries []fileCheckpointEntry
}

// CandidateVerification reports whether a candidate patch passed local checks.
type CandidateVerification struct {
	Passed            bool
	Strength          int
	HasTests          bool
	QuickCheckPlans   []ExecPlan
	QuickCheckResults []ExecResult
	QuickCheckError   string
	DepsPlan          *ExecPlan
	DepsSkipped       bool
	DepsResult        *ExecResult
	DepsError         string
	TestsPlan         *ExecPlan
	TestsResult       *ExecResult
	TestsError        string
}

// WriteFiles writes a batch of extracted files into the project.
func (p *Project) WriteFiles(files []ExtractedFile) error {
	for _, f := range files {
		if err := p.WriteFile(f.Path, f.Content); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return nil
}

// CheckpointFiles snapshots the current contents of the target files.
func (p *Project) CheckpointFiles(files []ExtractedFile) (*FileCheckpoint, error) {
	unique := make(map[string]struct{}, len(files))
	entries := make([]fileCheckpointEntry, 0, len(files))
	for _, f := range files {
		if _, seen := unique[f.Path]; seen {
			continue
		}
		unique[f.Path] = struct{}{}

		content, err := p.ReadFile(f.Path)
		if err == nil {
			entries = append(entries, fileCheckpointEntry{
				Path:    f.Path,
				Existed: true,
				Content: content,
			})
			continue
		}
		entries = append(entries, fileCheckpointEntry{
			Path:    f.Path,
			Existed: false,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return &FileCheckpoint{
		project: p,
		entries: entries,
	}, nil
}

// Restore rolls the project files back to the checkpointed state.
func (c *FileCheckpoint) Restore() error {
	if c == nil || c.project == nil {
		return nil
	}

	for _, entry := range c.entries {
		if entry.Existed {
			if err := c.project.WriteFile(entry.Path, entry.Content); err != nil {
				return fmt.Errorf("restore %s: %w", entry.Path, err)
			}
			continue
		}

		fullPath, err := c.project.validatePath(entry.Path, true)
		if err != nil {
			return err
		}
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", entry.Path, err)
		}
	}
	return nil
}

// CloneToTemp copies the project into a temporary directory for isolated checks.
func (p *Project) CloneToTemp() (*Project, error) {
	tempDir, err := os.MkdirTemp("", "makewand-candidate-*")
	if err != nil {
		return nil, fmt.Errorf("create temp workspace: %w", err)
	}

	if err := filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(p.Path, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if shouldIgnore(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		target := filepath.Join(tempDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	}); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("copy project to temp workspace: %w", err)
	}

	cloned, err := OpenProject(tempDir)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	return cloned, nil
}

// EvaluateCandidateFiles applies files in a temporary workspace and runs
// restricted dependency/test verification there.
func (p *Project) EvaluateCandidateFiles(ctx context.Context, files []ExtractedFile) (CandidateVerification, error) {
	clone, err := p.CloneToTemp()
	if err != nil {
		return CandidateVerification{}, err
	}
	defer os.RemoveAll(clone.Path)

	if err := clone.WriteFiles(files); err != nil {
		return CandidateVerification{}, err
	}
	if err := clone.ScanFiles(); err != nil {
		return CandidateVerification{}, err
	}

	return clone.VerifyRestrictedWorkspace(ctx, files), nil
}

// ChangedFilesAgainst returns the added/modified files in p compared with base.
// Deletions are not represented because ExtractedFile has no delete marker.
func (p *Project) ChangedFilesAgainst(base *Project) ([]ExtractedFile, error) {
	if p == nil || base == nil {
		return nil, nil
	}
	if err := p.ScanFiles(); err != nil {
		return nil, err
	}

	var files []ExtractedFile
	for _, entry := range p.Files {
		if entry.IsDir || entry.Path == "." {
			continue
		}
		content, err := p.ReadFile(entry.Path)
		if err != nil {
			return nil, err
		}
		baseContent, err := base.ReadFile(entry.Path)
		if err == nil && baseContent == content {
			continue
		}
		files = append(files, ExtractedFile{
			Path:    entry.Path,
			Content: content,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// VerifyRestrictedWorkspace runs layered candidate verification in-project.
func (p *Project) VerifyRestrictedWorkspace(ctx context.Context, files []ExtractedFile) CandidateVerification {
	report := CandidateVerification{}

	if !p.runInlineQuickChecks(files, &report) {
		return report
	}
	if !p.runExternalQuickChecks(ctx, files, &report) {
		return report
	}

	depsPlan, depsErr := p.DetectInstallPlan()
	if depsPlan != nil {
		report.DepsPlan = depsPlan
	}
	if depsErr != nil {
		report.DepsError = depsErr.Error()
		return report
	}
	if depsPlan != nil && shouldRunDependencyPlan(*depsPlan, files) {
		result, err := p.RunRestrictedPlan(ctx, *depsPlan)
		if result != nil {
			report.DepsResult = result
		}
		if err != nil {
			report.DepsError = err.Error()
			return report
		}
		if result != nil && result.ExitCode != 0 {
			report.DepsError = execFailureDetail(result)
			return report
		}
	} else if depsPlan != nil {
		report.DepsSkipped = true
	}

	testsPlan, testsErr := p.DetectTestPlan()
	if testsPlan != nil {
		report.TestsPlan = testsPlan
		report.HasTests = true
	}
	if testsErr != nil {
		report.TestsError = testsErr.Error()
		return report
	}
	if testsPlan != nil {
		result, err := p.RunRestrictedPlan(ctx, *testsPlan)
		if result != nil {
			report.TestsResult = result
		}
		if err != nil {
			report.TestsError = err.Error()
			return report
		}
		if result != nil && result.ExitCode != 0 {
			report.TestsError = execFailureDetail(result)
			return report
		}
		report.Passed = true
		report.Strength = 2
		return report
	}

	report.Passed = true
	report.Strength = 1
	return report
}

func (p *Project) runInlineQuickChecks(files []ExtractedFile, report *CandidateVerification) bool {
	if report == nil {
		return false
	}

	for _, f := range files {
		switch filepath.Ext(strings.ToLower(f.Path)) {
		case ".go":
			fset := token.NewFileSet()
			if _, err := parser.ParseFile(fset, f.Path, f.Content, parser.AllErrors); err != nil {
				report.QuickCheckError = fmt.Sprintf("quick check failed for %s: %v", f.Path, err)
				return false
			}
		case ".json":
			if filepath.Base(f.Path) != "package.json" {
				continue
			}
			var pkg any
			if err := json.Unmarshal([]byte(f.Content), &pkg); err != nil {
				report.QuickCheckError = fmt.Sprintf("quick check failed for %s: %v", f.Path, err)
				return false
			}
		}
	}

	return true
}

func (p *Project) runExternalQuickChecks(ctx context.Context, files []ExtractedFile, report *CandidateVerification) bool {
	if report == nil {
		return false
	}

	for _, plan := range detectQuickCheckPlans(files) {
		if !commandAvailable(plan.Command) {
			continue
		}
		report.QuickCheckPlans = append(report.QuickCheckPlans, plan)
		result, err := p.RunRestrictedPlan(ctx, plan)
		if result != nil {
			report.QuickCheckResults = append(report.QuickCheckResults, *result)
		}
		if err != nil {
			report.QuickCheckError = err.Error()
			return false
		}
		if result != nil && result.ExitCode != 0 {
			report.QuickCheckError = execFailureDetail(result)
			return false
		}
	}

	return true
}

func detectQuickCheckPlans(files []ExtractedFile) []ExecPlan {
	changedPy := make([]string, 0, len(files))
	changedJS := make([]string, 0, len(files))
	hasRustChanges := false

	for _, f := range files {
		switch strings.ToLower(filepath.Ext(f.Path)) {
		case ".py":
			changedPy = append(changedPy, f.Path)
		case ".js", ".mjs", ".cjs":
			changedJS = append(changedJS, f.Path)
		case ".rs":
			hasRustChanges = true
		}
		switch filepath.Base(f.Path) {
		case "Cargo.toml", "Cargo.lock":
			hasRustChanges = true
		}
	}

	var plans []ExecPlan
	if len(changedPy) > 0 {
		plans = append(plans, ExecPlan{
			Kind:     "quickcheck",
			Detector: "python files",
			Command:  "python3",
			Args:     append([]string{"-m", "py_compile"}, changedPy...),
		})
	}
	for _, path := range changedJS {
		plans = append(plans, ExecPlan{
			Kind:     "quickcheck",
			Detector: path,
			Command:  "node",
			Args:     []string{"--check", path},
		})
	}
	if hasRustChanges {
		plans = append(plans, ExecPlan{
			Kind:     "quickcheck",
			Detector: "rust workspace",
			Command:  "cargo",
			Args:     []string{"check", "--tests"},
		})
	}

	return plans
}

func shouldRunDependencyPlan(plan ExecPlan, files []ExtractedFile) bool {
	changed := make(map[string]struct{}, len(files))
	for _, f := range files {
		changed[f.Path] = struct{}{}
	}

	switch {
	case plan.Command == "go" && slicesEqual(plan.Args, []string{"mod", "tidy"}):
		return changedPath(changed, "go.mod", "go.sum")
	case plan.Command == "cargo" && slicesEqual(plan.Args, []string{"build"}):
		return changedPath(changed, "Cargo.toml", "Cargo.lock")
	default:
		return true
	}
}

func changedPath(changed map[string]struct{}, paths ...string) bool {
	for _, path := range paths {
		if _, ok := changed[path]; ok {
			return true
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func commandAvailable(command string) bool {
	_, err := exec.LookPath(command)
	return err == nil
}

func execFailureDetail(result *ExecResult) string {
	if result == nil {
		return ""
	}
	msg := strings.TrimSpace(result.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(result.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("command exited with status %d", result.ExitCode)
	}
	return msg
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
