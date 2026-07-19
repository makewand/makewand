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
	"regexp"
	"slices"
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
	Isolated          bool     // external commands ran inside the bubblewrap sandbox
	IsolationError    string   // why verification could not execute commands (fail closed)
	NoTestsRan        bool     // the test command succeeded but executed no actual tests
	BaselineTests     bool     // the baseline workspace defined its own test plan
	RestoredTests     []string // baseline test files restored before verification
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
// isolated dependency/test verification there.
//
// Baseline test trust: candidate rewrites of pre-existing test files are NOT
// applied to the verification clone - the clone keeps the baseline content, so
// a candidate cannot pass by weakening or deleting the tests that judge it.
// (Deletions need no special handling here: the clone is a fresh copy of the
// baseline, so files the candidate deleted in its own workspace are still
// present.) New test files added by the candidate are written, but strength
// scoring in verifyRestrictedWorkspace ensures they cannot raise Strength on
// their own.
func (p *Project) EvaluateCandidateFiles(ctx context.Context, files []ExtractedFile) (CandidateVerification, error) {
	clone, err := p.CloneToTemp()
	if err != nil {
		return CandidateVerification{}, err
	}
	defer os.RemoveAll(clone.Path)

	applied, restored := p.splitBaselineTestOverwrites(files)
	if err := clone.WriteFiles(applied); err != nil {
		return CandidateVerification{}, err
	}
	// A candidate can edit package.json to weaken scripts.test (e.g. set it to
	// "true") and earn a Strength-2 pass. Restore the baseline test script in the
	// clone - like *_test.go files are restored - so the trusted baseline command
	// is what actually runs.
	if restoredScript, err := p.restoreBaselineNpmTestScript(clone); err != nil {
		return CandidateVerification{}, err
	} else if restoredScript != "" {
		restored = append(restored, restoredScript)
		sort.Strings(restored)
	}
	if err := clone.ScanFiles(); err != nil {
		return CandidateVerification{}, err
	}

	report := clone.verifyRestrictedWorkspace(ctx, applied, p.baselineTrustedTests())
	report.RestoredTests = restored
	return report, nil
}

// splitBaselineTestOverwrites partitions candidate files into the set that may
// be applied to the verification clone and the pre-existing test files whose
// baseline content must be kept instead.
func (p *Project) splitBaselineTestOverwrites(files []ExtractedFile) (applied []ExtractedFile, restored []string) {
	applied = make([]ExtractedFile, 0, len(files))
	for _, f := range files {
		if isBaselineTestFile(f.Path) {
			if baseline, err := p.ReadFile(f.Path); err == nil {
				if baseline != f.Content {
					restored = append(restored, f.Path)
				}
				continue // keep the baseline copy already present in the clone
			}
		}
		applied = append(applied, f)
	}
	sort.Strings(restored)
	return applied, restored
}

// isBaselineTestFile reports whether a path looks like a test definition that
// verification must trust from the baseline rather than from the candidate.
func isBaselineTestFile(path string) bool {
	base := strings.ToLower(filepath.Base(filepath.ToSlash(path)))
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, "_test.py"),
		strings.HasSuffix(base, ".test.js"),
		strings.HasSuffix(base, ".test.ts"),
		strings.HasSuffix(base, ".spec.js"),
		strings.HasSuffix(base, ".spec.ts"),
		base == "conftest.py",
		base == "pytest.ini":
		return true
	}
	return false
}

// baselineTrustedTests reports whether the baseline project defines tests of
// its own that verification may trust for Strength >= 2. Config-driven runners
// (npm test script, pytest.ini) are explicit baseline verification commands;
// file-driven runners like "go test" additionally require baseline test files,
// otherwise a candidate could raise Strength just by shipping its own tests.
// An npm test script that is trivially always-successful (true, exit 0, echo,
// empty) is not a real test suite, so it never earns Strength >= 2.
func (p *Project) baselineTrustedTests() bool {
	plan, err := p.DetectTestPlan()
	if err != nil || plan == nil {
		return false
	}
	switch plan.Command {
	case "go":
		return p.containsBaselineTestFiles()
	case "npm", "pnpm", "yarn":
		return p.baselineHasRealNpmTestScript()
	}
	return true
}

// baselineHasRealNpmTestScript reports whether the baseline package.json defines
// a non-trivial test script.
func (p *Project) baselineHasRealNpmTestScript() bool {
	content, err := p.ReadFile("package.json")
	if err != nil {
		return false
	}
	return !isTrivialTestScript(packageJSONTestScript(content))
}

// restoreBaselineNpmTestScript rewrites the clone's package.json test script to
// the baseline's when a candidate changed it, so a candidate cannot earn a
// Strength-2 pass by weakening scripts.test. It returns the restored path (empty
// when the baseline has no npm test script or the candidate left it unchanged).
func (p *Project) restoreBaselineNpmTestScript(clone *Project) (string, error) {
	baseContent, err := p.ReadFile("package.json")
	if err != nil {
		return "", nil // baseline has no package.json to protect
	}
	baseScript := packageJSONTestScript(baseContent)
	if baseScript == "" {
		return "", nil // baseline defines no npm test script
	}
	cloneContent, err := clone.ReadFile("package.json")
	if err != nil {
		return "", nil // candidate removed package.json (deletion surfaced elsewhere)
	}
	if packageJSONTestScript(cloneContent) == baseScript {
		return "", nil // unchanged
	}
	patched, err := setPackageJSONTestScript(cloneContent, baseScript)
	if err != nil {
		// The candidate package.json is unparseable; runInlineQuickChecks already
		// rejects invalid package.json, so leave it for that gate to catch.
		return "", nil
	}
	if err := clone.WriteFile("package.json", patched); err != nil {
		return "", err
	}
	return "package.json", nil
}

// packageJSONTestScript extracts the trimmed scripts.test entry, or "" when it
// is absent or the content is not valid package.json.
func packageJSONTestScript(content string) string {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal([]byte(content), &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Scripts["test"])
}

// setPackageJSONTestScript returns package.json content with scripts.test set to
// script, preserving the candidate's other fields (e.g. added dependencies).
func setPackageJSONTestScript(content, script string) (string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		return "", err
	}
	scripts := map[string]json.RawMessage{}
	if raw, ok := root["scripts"]; ok {
		if err := json.Unmarshal(raw, &scripts); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(script)
	if err != nil {
		return "", err
	}
	scripts["test"] = encoded
	scriptsRaw, err := json.Marshal(scripts)
	if err != nil {
		return "", err
	}
	root["scripts"] = scriptsRaw
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// shellCmdKind classifies a single shell command for control-flow-aware trivial
// test-script detection.
type shellCmdKind int

const (
	// shellCmdRealRunner runs real tests when executed.
	shellCmdRealRunner shellCmdKind = iota
	shellCmdSuccess                 // always-success no-op (true, :, echo, ...)
	shellCmdFailure                 // always-failure no-op (false)
	shellCmdExit                    // exit / exit N: terminates the script
)

// shellSegment is one command in a script plus the operator that precedes it.
type shellSegment struct {
	op   string // "", "&&", "||", ";", "|", "&"
	text string
}

// splitShellSegments splits a command line on the control operators &&, ||, ;,
// | and & while respecting single and double quotes, so operators inside quoted
// strings are not treated as separators. Each segment carries the operator that
// precedes it ("" for the first).
func splitShellSegments(s string) []shellSegment {
	var (
		segs   []shellSegment
		buf    strings.Builder
		op     string
		single bool
		double bool
	)
	flush := func(next string) {
		segs = append(segs, shellSegment{op: op, text: buf.String()})
		buf.Reset()
		op = next
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case single:
			if c == '\'' {
				single = false
			}
			buf.WriteByte(c)
		case double:
			if c == '"' {
				double = false
			}
			buf.WriteByte(c)
		case c == '\'':
			single = true
			buf.WriteByte(c)
		case c == '"':
			double = true
			buf.WriteByte(c)
		case c == '&' && i+1 < len(s) && s[i+1] == '&':
			flush("&&")
			i++
		case c == '|' && i+1 < len(s) && s[i+1] == '|':
			flush("||")
			i++
		case c == ';':
			flush(";")
		case c == '|':
			flush("|")
		case c == '&':
			flush("&")
		default:
			buf.WriteByte(c)
		}
	}
	segs = append(segs, shellSegment{op: op, text: buf.String()})
	return segs
}

// classifyShellCommand classifies a single command (with no control operators).
func classifyShellCommand(segment string) shellCmdKind {
	seg := strings.TrimSpace(segment)
	if seg == "" {
		return shellCmdSuccess
	}
	lower := strings.ToLower(seg)
	if lower == "exit" {
		return shellCmdExit
	}
	// `exit`, `exit 0`, `exit 1`, ... terminate the script and run no tests.
	if rest, ok := strings.CutPrefix(lower, "exit "); ok {
		if rest = strings.TrimSpace(rest); rest == "" || isAllDigits(rest) {
			return shellCmdExit
		}
	}
	switch lower {
	case "true", ":", "/bin/true":
		return shellCmdSuccess
	case "false", "/bin/false":
		return shellCmdFailure
	}
	// Pure echo statements (including the npm "no test specified" placeholder)
	// do nothing meaningful.
	if lower == "echo" || strings.HasPrefix(lower, "echo ") {
		return shellCmdSuccess
	}
	return shellCmdRealRunner
}

// isTrivialTestScript reports whether a test command does no real testing: it is
// empty, or no real test runner is ever actually executed once shell control
// flow is taken into account. It is control-flow aware: `true || jest` never runs
// jest (|| short-circuits) and `exit 0 && jest` terminates first, so both are
// trivial; `jest || true` and `true && jest` do run jest, so both are not. Quoted
// operators are not treated as control flow. The safe direction is to treat a
// script as "no real tests" unless a real runner positively executes, so it never
// earns a Strength-2 verification pass.
func isTrivialTestScript(script string) bool {
	s := strings.TrimSpace(script)
	if s == "" {
		return true
	}
	lastSuccess := true
	terminated := false
	for i, seg := range splitShellSegments(s) {
		var runs bool
		switch {
		case terminated:
			runs = false
		case i == 0:
			runs = true
		case seg.op == "&&":
			runs = lastSuccess
		case seg.op == "||":
			runs = !lastSuccess
		default: // ";", "|", "&": no short-circuit, the command runs
			runs = true
		}
		if !runs {
			continue
		}
		switch classifyShellCommand(seg.text) {
		case shellCmdRealRunner:
			return false // a real test runner actually executes
		case shellCmdExit:
			terminated = true
		case shellCmdFailure:
			lastSuccess = false
		default: // shellCmdSuccess
			lastSuccess = true
		}
	}
	return true
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (p *Project) containsBaselineTestFiles() bool {
	for _, entry := range p.Files {
		if !entry.IsDir && isBaselineTestFile(entry.Path) {
			return true
		}
	}
	return false
}

// ChangedFilesAgainst returns the added/modified files in p compared with base.
// Deletions are not represented because ExtractedFile has no delete marker; use
// ChangedFilesAgainstWithDeletions to observe them.
func (p *Project) ChangedFilesAgainst(base *Project) ([]ExtractedFile, error) {
	files, _, err := p.ChangedFilesAgainstWithDeletions(base)
	return files, err
}

// ChangedFilesAgainstWithDeletions returns the added/modified files in p
// compared with base, plus the relative paths of baseline files that no longer
// exist in p. Callers must surface deletions to the user; they are never
// silently dropped.
func (p *Project) ChangedFilesAgainstWithDeletions(base *Project) ([]ExtractedFile, []string, error) {
	if p == nil || base == nil {
		return nil, nil, nil
	}
	if err := p.ScanFiles(); err != nil {
		return nil, nil, err
	}

	present := make(map[string]struct{}, len(p.Files))
	var files []ExtractedFile
	for _, entry := range p.Files {
		if entry.IsDir || entry.Path == "." {
			continue
		}
		present[entry.Path] = struct{}{}
		content, err := p.ReadFile(entry.Path)
		if err != nil {
			return nil, nil, err
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

	// base.Files is a read-only snapshot here; do not rescan base because it may
	// be shared across concurrent candidate attempts.
	var deleted []string
	for _, entry := range base.Files {
		if entry.IsDir || entry.Path == "." {
			continue
		}
		if _, ok := present[entry.Path]; !ok {
			deleted = append(deleted, entry.Path)
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	sort.Strings(deleted)
	return files, deleted, nil
}

// mergeCandidateFiles unions model-reported FILE blocks with the clone diff.
// The clone diff is authoritative: when both describe the same path, the clone
// content wins because it is what verification actually observed on disk.
func mergeCandidateFiles(reported, cloneDiff []ExtractedFile) []ExtractedFile {
	if len(cloneDiff) == 0 {
		return reported
	}
	merged := make(map[string]string, len(reported)+len(cloneDiff))
	for _, f := range reported {
		merged[f.Path] = f.Content
	}
	for _, f := range cloneDiff {
		merged[f.Path] = f.Content
	}

	out := make([]ExtractedFile, 0, len(merged))
	for path, content := range merged {
		out = append(out, ExtractedFile{Path: path, Content: content})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// extractedFilesEqual compares two file sets by path and content, ignoring
// order and duplicate paths (last occurrence wins, matching write semantics).
func extractedFilesEqual(a, b []ExtractedFile) bool {
	toMap := func(files []ExtractedFile) map[string]string {
		m := make(map[string]string, len(files))
		for _, f := range files {
			m[f.Path] = f.Content
		}
		return m
	}
	am, bm := toMap(a), toMap(b)
	if len(am) != len(bm) {
		return false
	}
	for path, content := range am {
		if other, ok := bm[path]; !ok || other != content {
			return false
		}
	}
	return true
}

// VerifyRestrictedWorkspace runs layered candidate verification in-project.
func (p *Project) VerifyRestrictedWorkspace(ctx context.Context, files []ExtractedFile) CandidateVerification {
	return p.verifyRestrictedWorkspace(ctx, files, p.baselineTrustedTests())
}

func (p *Project) verifyRestrictedWorkspace(ctx context.Context, files []ExtractedFile, baselineHasTests bool) CandidateVerification {
	report := CandidateVerification{BaselineTests: baselineHasTests}

	if !p.runInlineQuickChecks(files, &report) {
		return report
	}

	// Fail closed: everything below executes candidate-influenced commands, so
	// without working sandbox isolation (or the explicit unsafe host opt-in)
	// nothing runs and the candidate stays unverified.
	execEnv, isoErr := resolveVerifyExecEnvironment()
	if isoErr != nil {
		report.IsolationError = isoErr.Error()
		return report
	}
	report.Isolated = execEnv.mode == verifyExecIsolated

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
		result, err := p.RunVerificationPlan(ctx, *depsPlan)
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
		result, err := p.RunVerificationPlan(ctx, *testsPlan)
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
		report.NoTestsRan = detectNoTestsRun(*testsPlan, result)
		// Strength 2 requires baseline-trusted tests to have actually run and
		// passed. A candidate that merely compiles, or whose only tests are the
		// ones it wrote itself, caps at Strength 1.
		if !report.NoTestsRan && baselineHasTests {
			report.Strength = 2
		} else {
			report.Strength = 1
		}
		return report
	}

	report.Passed = true
	report.Strength = 1
	return report
}

var cargoRunningTestsRe = regexp.MustCompile(`(?m)^running (\d+) tests?`)

// npmZeroTestsRe matches a Mocha-style "0 passing" summary while avoiding false
// positives from larger counts such as "10 passing".
var npmZeroTestsRe = regexp.MustCompile(`(?m)(^|[^0-9])0 passing`)

// nodeZeroTestsRe matches node --test's TAP/spec summary when it ran no tests
// ("tests 0", "# tests 0", or "0 tests"), anchored so counts such as "10 tests"
// and "tests 10" do not match.
var nodeZeroTestsRe = regexp.MustCompile(`(?m)(^|[^0-9a-z])(tests 0|0 tests)([^0-9a-z]|$)`)

// detectNoTestsRun reports whether a successful test command executed zero
// actual tests (e.g. "go test" over packages with no test files).
func detectNoTestsRun(plan ExecPlan, result *ExecResult) bool {
	if result == nil {
		return true
	}
	output := result.Stdout + "\n" + result.Stderr
	switch plan.Command {
	case "go":
		return !goTestOutputRanTests(result.Stdout)
	case "pytest":
		return strings.Contains(output, "no tests ran")
	case "npm", "pnpm", "yarn":
		lower := strings.ToLower(output)
		if strings.Contains(lower, "no test specified") ||
			strings.Contains(lower, "no tests found") ||
			strings.Contains(lower, "no tests to run") {
			return true
		}
		// Mocha reports "0 passing" when nothing ran; node --test reports
		// "tests 0"/"# tests 0"/"0 tests". Anchor so "10 passing", "10 tests" and
		// similar counts do not match.
		return npmZeroTestsRe.MatchString(output) || nodeZeroTestsRe.MatchString(lower)
	case "cargo":
		ran := false
		for _, match := range cargoRunningTestsRe.FindAllStringSubmatch(output, -1) {
			if match[1] != "0" {
				ran = true
			}
		}
		return !ran
	default:
		return false
	}
}

func goTestOutputRanTests(stdout string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "ok" && !strings.Contains(line, "no tests to run") {
			return true
		}
	}
	return false
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
		result, err := p.RunVerificationPlan(ctx, plan)
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
	return slices.Equal(a, b)
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
