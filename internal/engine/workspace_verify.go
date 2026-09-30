package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// CreatedDirs lists the parent directories (project-relative, deepest
	// first) that did not exist at checkpoint time. WriteFiles creates them for
	// a new file; Restore removes them again when they are left empty.
	CreatedDirs []string
	Content     string
	BackupPath  string
	Mode        os.FileMode
	Nlink       uint64
	Dev         uint64
	Ino         uint64
}

// FileCheckpoint stores the pre-change state for a set of project files.
type FileCheckpoint struct {
	project *Project
	entries []fileCheckpointEntry
}

// BackupPaths returns the list of active backup file paths on disk.
func (c *FileCheckpoint) BackupPaths() []string {
	if c == nil {
		return nil
	}
	var paths []string
	for _, entry := range c.entries {
		if entry.BackupPath != "" {
			if _, err := os.Stat(entry.BackupPath); err == nil {
				paths = append(paths, entry.BackupPath)
			}
		}
	}
	return paths
}

// CandidateVerification reports whether a candidate patch passed local checks.
type CandidateVerification struct {
	Passed            bool
	Strength          int
	HasTests          bool
	Isolated          bool     // external commands ran inside the bubblewrap sandbox
	IsolationError    string   // why verification could not execute commands (fail closed)
	EnvironmentError  bool     // a check could not run for environment reasons (sandbox setup, host toolchain), not because the candidate failed it
	NoTestsRan        bool     // the test command succeeded but executed no actual tests
	NoTestPlan        bool     // the baseline defines no test command: only quick checks ran, nothing was verified
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
	IntegrityError    string          // input tree changed during verification
	EvidenceLimit     string          // successful candidate-controlled output is not an independent attestation
	VerifiedFiles     []ExtractedFile // exact files verified in the tree
	VerifiedContent   string          // rendered payload of verified files
	VerifiedDigest    string          // sha256 hex digest of verified files
}

// WriteFiles writes a batch of extracted files into the project.
func (p *Project) WriteFiles(files []ExtractedFile) error {
	for _, f := range files {
		var mode *fs.FileMode
		if f.ModeKnown {
			mode = &f.Mode
		}
		if err := p.writeFileMode(f.Path, f.Content, mode); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return nil
}

// CheckpointFiles snapshots the current contents of the target files.
func (p *Project) CheckpointFiles(files []ExtractedFile) (*FileCheckpoint, error) {
	unique := make(map[string]struct{}, len(files))
	entries := make([]fileCheckpointEntry, 0, len(files))

	cleanupOnError := func() {
		for _, e := range entries {
			if e.BackupPath != "" {
				_ = os.Remove(e.BackupPath)
			}
		}
	}

	for _, f := range files {
		if _, seen := unique[f.Path]; seen {
			continue
		}
		unique[f.Path] = struct{}{}

		fullPath, err := p.validatePath(f.Path, true)
		if err != nil {
			cleanupOnError()
			return nil, err
		}

		info, statErr := os.Lstat(fullPath)
		if statErr == nil {
			if info.IsDir() {
				continue
			}
			mode := info.Mode()
			identity, identityErr := checkpointIdentity(fullPath, info)
			if identityErr != nil {
				cleanupOnError()
				return nil, fmt.Errorf("identify checkpoint %s: %w", f.Path, identityErr)
			}
			// If file size is within maxReadFileSize, read into Content
			if info.Size() <= maxReadFileSize {
				data, readErr := os.ReadFile(fullPath)
				if readErr == nil {
					entries = append(entries, fileCheckpointEntry{
						Path:    f.Path,
						Existed: true,
						Content: string(data),
						Mode:    mode,
						Nlink:   identity.Nlink,
						Dev:     identity.Dev,
						Ino:     identity.Ino,
					})
					continue
				}
			}
			// For files exceeding maxReadFileSize or failing memory read, preserve a temporary backup on disk
			backupFile, createErr := os.CreateTemp("", "makewand-chkpt-*")
			if createErr != nil {
				cleanupOnError()
				return nil, fmt.Errorf("create backup for %s: %w", f.Path, createErr)
			}
			srcFile, openErr := os.Open(fullPath)
			if openErr != nil {
				_ = backupFile.Close()
				_ = os.Remove(backupFile.Name())
				cleanupOnError()
				return nil, fmt.Errorf("open backup source %s: %w", f.Path, openErr)
			}
			_, copyErr := io.Copy(backupFile, srcFile)
			srcCloseErr := srcFile.Close()
			dstCloseErr := backupFile.Close()
			if copyErr != nil || srcCloseErr != nil || dstCloseErr != nil {
				_ = os.Remove(backupFile.Name())
				cleanupOnError()
				return nil, fmt.Errorf("copy backup %s: copy=%v, srcClose=%v, dstClose=%v", f.Path, copyErr, srcCloseErr, dstCloseErr)
			}
			entries = append(entries, fileCheckpointEntry{
				Path:       f.Path,
				Existed:    true,
				BackupPath: backupFile.Name(),
				Mode:       mode,
				Nlink:      identity.Nlink,
				Dev:        identity.Dev,
				Ino:        identity.Ino,
			})
			continue
		}

		if os.IsNotExist(statErr) {
			entries = append(entries, fileCheckpointEntry{
				Path:        f.Path,
				Existed:     false,
				CreatedDirs: p.missingParentDirs(f.Path),
			})
			continue
		}

		cleanupOnError()
		return nil, fmt.Errorf("stat %s: %w", f.Path, statErr)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return &FileCheckpoint{
		project: p,
		entries: entries,
	}, nil
}

// Cleanup removes any temporary backup files created for large file checkpoints.
func (c *FileCheckpoint) Cleanup() {
	if c == nil {
		return
	}
	for _, entry := range c.entries {
		if entry.BackupPath != "" {
			_ = os.Remove(entry.BackupPath)
		}
	}
}

func (c *FileCheckpoint) findInternalHardlinkSibling(targetPath string, dev, ino uint64) string {
	if c == nil || c.project == nil || ino == 0 {
		return ""
	}
	var siblingPath string
	_ = filepath.WalkDir(c.project.Path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			if d != nil && d.IsDir() && shouldIgnoreSet[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if p == targetPath {
			return nil
		}
		if fi, statErr := os.Lstat(p); statErr == nil {
			if identity, err := checkpointIdentity(p, fi); err == nil {
				if identity.Dev == dev && identity.Ino == ino {
					siblingPath = p
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	return siblingPath
}

// Restore rolls the project files back to the checkpointed state.
func (c *FileCheckpoint) Restore() error {
	if c == nil || c.project == nil {
		return nil
	}

	for _, entry := range c.entries {
		if entry.Existed {
			if entry.BackupPath != "" {
				fullPath, err := c.project.validatePath(entry.Path, true)
				if err != nil {
					return err
				}
				dir := filepath.Dir(fullPath)
				if err := os.MkdirAll(dir, 0755); err != nil {
					return fmt.Errorf("ensure dir for restore %s: %w", entry.Path, err)
				}
				fullPath, err = c.project.validatePath(entry.Path, true)
				if err != nil {
					return err
				}
				dir = filepath.Dir(fullPath)

				src, err := os.Open(entry.BackupPath)
				if err != nil {
					return fmt.Errorf("restore %s from backup (%s): %w", entry.Path, entry.BackupPath, err)
				}

				// If file was part of an internal hard link group within the workspace,
				// check if another file in the workspace still holds that exact (Dev, Ino).
				// If so, re-establish the hardlink to preserve internal consistency without mutating external files.
				if entry.Nlink > 1 && entry.Ino != 0 {
					sibling := c.findInternalHardlinkSibling(fullPath, entry.Dev, entry.Ino)
					if sibling != "" {
						_ = os.Remove(fullPath)
						if err := os.Link(sibling, fullPath); err == nil {
							dst, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_TRUNC, entry.Mode.Perm())
							if err == nil {
								_, copyErr := io.Copy(dst, src)
								_ = src.Close()
								_ = dst.Close()
								if copyErr == nil {
									if entry.Mode != 0 {
										_ = os.Chmod(fullPath, entry.Mode.Perm())
									}
									continue
								}
							}
						}
					}
				}

				// Atomic replace: write to temp file in target directory, then rename
				tmpDst, err := os.CreateTemp(dir, ".makewand-restore-*")
				if err != nil {
					src.Close()
					return fmt.Errorf("create restore temp for %s: %w", entry.Path, err)
				}
				_, copyErr := io.Copy(tmpDst, src)
				srcCloseErr := src.Close()
				closeErr := tmpDst.Close()
				if copyErr != nil || srcCloseErr != nil || closeErr != nil {
					_ = os.Remove(tmpDst.Name())
					return fmt.Errorf("copy restore %s from %s: copy=%v, srcClose=%v, close=%v", entry.Path, entry.BackupPath, copyErr, srcCloseErr, closeErr)
				}
				// Preserve original file permissions (including executable bits)
				if entry.Mode != 0 {
					if err := os.Chmod(tmpDst.Name(), entry.Mode.Perm()); err != nil {
						_ = os.Remove(tmpDst.Name())
						return fmt.Errorf("chmod restore %s: %w", entry.Path, err)
					}
				}
				if err := os.Rename(tmpDst.Name(), fullPath); err != nil {
					_ = os.Remove(tmpDst.Name())
					return fmt.Errorf("rename restore %s: %w", entry.Path, err)
				}
				continue
			}

			if entry.Nlink > 1 && entry.Ino != 0 {
				fullPath, err := c.project.validatePath(entry.Path, true)
				if err == nil {
					sibling := c.findInternalHardlinkSibling(fullPath, entry.Dev, entry.Ino)
					if sibling != "" {
						_ = os.Remove(fullPath)
						if err := os.Link(sibling, fullPath); err == nil {
							if err := os.WriteFile(fullPath, []byte(entry.Content), entry.Mode.Perm()); err == nil {
								continue
							}
						}
					}
				}
			}

			if err := c.project.WriteFile(entry.Path, entry.Content); err != nil {
				return fmt.Errorf("restore %s: %w", entry.Path, err)
			}
			if entry.Mode != 0 {
				fullPath, err := c.project.validatePath(entry.Path, true)
				if err != nil {
					return fmt.Errorf("validate path for chmod %s: %w", entry.Path, err)
				}
				if err := os.Chmod(fullPath, entry.Mode.Perm()); err != nil {
					return fmt.Errorf("chmod restore %s: %w", entry.Path, err)
				}
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
		c.removeCreatedDirs(entry.CreatedDirs)
	}

	// Only clean up temporary backups when ALL files have been successfully restored!
	c.Cleanup()
	return nil
}

// missingParentDirs returns the parent directories of relPath that do not
// exist yet, deepest first.
func (p *Project) missingParentDirs(relPath string) []string {
	var missing []string
	dir := filepath.Dir(filepath.Clean(relPath))
	for dir != "." && dir != string(filepath.Separator) && dir != "" {
		if _, err := os.Lstat(filepath.Join(p.Path, dir)); err == nil {
			break
		} else if !os.IsNotExist(err) {
			break
		}
		missing = append(missing, dir)
		dir = filepath.Dir(dir)
	}
	return missing
}

// removeCreatedDirs removes directories a rolled-back write created, deepest
// first, stopping at the first one that is not empty (it holds other files
// now) or cannot be removed. Root-confined so a swapped symlink cannot
// redirect the removal.
func (c *FileCheckpoint) removeCreatedDirs(dirs []string) {
	if len(dirs) == 0 {
		return
	}
	root, err := os.OpenRoot(c.project.Path)
	if err != nil {
		return
	}
	defer root.Close()
	for _, dir := range dirs {
		info, err := root.Lstat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return
		}
		if !info.IsDir() || root.Remove(dir) != nil {
			return
		}
	}
}

// CloneToTemp copies the project into a temporary directory for isolated checks.
func (p *Project) CloneToTemp() (*Project, error) {
	return p.cloneToTemp(nil)
}

func (p *Project) cloneToTemp(copyContents func(*os.File, *os.File) error) (*Project, error) {
	return p.cloneToTempContext(context.Background(), copyContents)
}

func (p *Project) cloneToTempContext(ctx context.Context, copyContents func(*os.File, *os.File) error) (*Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := os.OpenRoot(p.Path)
	if err != nil {
		return nil, fmt.Errorf("open source workspace: %w", err)
	}
	defer source.Close()

	tempDir, err := os.MkdirTemp("", "makewand-candidate-*")
	if err != nil {
		return nil, fmt.Errorf("create temp workspace: %w", err)
	}
	targetInfo, err := os.Stat(tempDir)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	capabilities := make(workspaceCopyCapabilities)
	if copyContents == nil {
		copyContents, err = selectWorkspaceCopier(p.Path, tempDir)
		if err != nil {
			_ = os.RemoveAll(tempDir)
			return nil, err
		}
	}

	if err := filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if !info.Mode().IsRegular() {
			return fmt.Errorf("workspace input is not a regular file: %s", rel)
		}
		in, err := source.Open(rel)
		if err != nil {
			return err
		}
		defer in.Close()
		opened, err := in.Stat()
		if err != nil {
			return err
		}
		if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			return fmt.Errorf("workspace input changed during copy: %s", rel)
		}
		copier := copyContents
		if copier == nil {
			copier = func(in, out *os.File) error { return capabilities.copy(in, out, opened, targetInfo) }
		}
		return copyWorkspaceFile(in, target, info.Mode().Perm(), copier)
	}); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("copy project to temp workspace: %w", err)
	}

	cloned, err := OpenProject(tempDir)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	// Verification clones execute the same restricted plans as the parent, so
	// they inherit the parent's host-execution authorization.
	cloned.unsafeHostAuth = p.unsafeHostAuth
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
	// "true") and skip the baseline checks. Restore the baseline test script in the
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

	// Resolve the execution contract from the trusted baseline before any
	// candidate-controlled metadata can choose a different runner.
	contract, err := p.baselineVerificationContract()
	if err != nil {
		return CandidateVerification{}, err
	}
	// Per-run canary tests (clone only, planted before the input digest so the
	// integrity check covers them) make blind test-output forgery detectable.
	if err := contract.plantGoCanaries(clone); err != nil {
		return CandidateVerification{}, err
	}
	sealed, err := snapshotCandidateFiles(clone, applied)
	if err != nil {
		return CandidateVerification{}, err
	}
	before, err := workspaceInputDigest(clone.Path)
	if err != nil {
		return CandidateVerification{}, err
	}
	report := clone.verifyRestrictedWorkspace(ctx, sealed, contract)
	report.RestoredTests = restored
	after, err := workspaceInputDigest(clone.Path)
	afterFiles, filesErr := snapshotCandidateFiles(clone, sealed)
	if err != nil || filesErr != nil || before != after || !extractedFilesEqual(sealed, afterFiles) {
		report.Passed = false
		report.Strength = 0
		report.IntegrityError = "verification changed its input files or permissions; review the generated changes and verify again"
		if err != nil {
			report.IntegrityError += ": " + err.Error()
		}
		return report, nil
	}
	// Seal the pre-execution bytes, never the mutable post-test workspace.
	if report.Passed {
		report.VerifiedFiles = sealed
		report.VerifiedContent = RenderExtractedFiles(sealed)
		report.VerifiedDigest = calculateFilesDigest(sealed)
	}

	return report, nil
}

func calculateFilesDigest(files []ExtractedFile) string {
	sortedFiles := make([]ExtractedFile, len(files))
	copy(sortedFiles, files)
	sort.Slice(sortedFiles, func(i, j int) bool {
		return sortedFiles[i].Path < sortedFiles[j].Path
	})
	hasher := sha256.New()
	encoder := json.NewEncoder(hasher)
	for _, f := range sortedFiles {
		_ = encoder.Encode(f)
	}

	return hex.EncodeToString(hasher.Sum(nil))
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
// its own. This describes the test plan, not a trusted acceptance attestation.
// Config-driven runners
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
	diff, err := p.DiffAgainst(base)
	return diff.Files, diff.Deleted, err
}

// CandidateDiff is the change set of a candidate workspace against its base.
type CandidateDiff struct {
	// Files are the added/modified files that can be verified and applied.
	Files []ExtractedFile
	// Deleted are baseline files missing from the candidate workspace.
	Deleted []string
	// LargeChanged are files over the size limit for verified changes
	// (maxReadFileSize) that the candidate added or modified. They cannot be
	// verified or applied and are reported instead of failing the candidate.
	LargeChanged []string
	// LargeUnchanged are files over the size limit that the candidate left
	// untouched (compared by streaming, never loaded into memory). They are
	// skipped and recorded.
	LargeUnchanged []string
}

// DiffAgainst compares the workspace p with base. Files larger than the
// verified-change limit never fail the diff: unchanged ones are skipped and
// recorded, changed ones are reported as not applicable.
func (p *Project) DiffAgainst(base *Project) (CandidateDiff, error) {
	var diff CandidateDiff
	if p == nil || base == nil {
		return diff, nil
	}
	if err := p.ScanFiles(); err != nil {
		return diff, err
	}

	present := make(map[string]struct{}, len(p.Files))
	for _, entry := range p.Files {
		if entry.IsDir || entry.Path == "." {
			continue
		}
		present[entry.Path] = struct{}{}
		fullPath := filepath.Join(p.Path, entry.Path)
		info, statErr := os.Lstat(fullPath)
		if statErr != nil {
			return CandidateDiff{}, statErr
		}
		basePath := filepath.Join(base.Path, entry.Path)
		baseInfo, baseStatErr := os.Lstat(basePath)
		if info.Mode().IsRegular() && info.Size() > maxReadFileSize {
			same := baseStatErr == nil && baseInfo.Mode().IsRegular() && baseInfo.Mode().Perm() == info.Mode().Perm()
			if same {
				equal, err := sameFileContent(fullPath, basePath)
				if err != nil {
					return CandidateDiff{}, err
				}
				same = equal
			}
			if same {
				diff.LargeUnchanged = append(diff.LargeUnchanged, entry.Path)
			} else {
				diff.LargeChanged = append(diff.LargeChanged, entry.Path)
			}
			continue
		}
		content, err := p.ReadFile(entry.Path)
		if err != nil {
			return CandidateDiff{}, err
		}
		baseContent, err := base.ReadFile(entry.Path)
		if err == nil && baseStatErr == nil && baseContent == content && baseInfo.Mode().Perm() == info.Mode().Perm() {
			continue
		}
		diff.Files = append(diff.Files, ExtractedFile{
			Path:    entry.Path,
			Content: content,
			Mode:    info.Mode().Perm(), ModeKnown: true,
		})
	}

	// base.Files is a read-only snapshot here; do not rescan base because it may
	// be shared across concurrent candidate attempts.
	for _, entry := range base.Files {
		if entry.IsDir || entry.Path == "." {
			continue
		}
		if _, ok := present[entry.Path]; !ok {
			diff.Deleted = append(diff.Deleted, entry.Path)
		}
	}

	sort.Slice(diff.Files, func(i, j int) bool {
		return diff.Files[i].Path < diff.Files[j].Path
	})
	sort.Strings(diff.Deleted)
	sort.Strings(diff.LargeChanged)
	sort.Strings(diff.LargeUnchanged)
	return diff, nil
}

// sameFileContent streams two files and reports whether their bytes match.
func sameFileContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	bufA := make([]byte, 64<<10)
	bufB := make([]byte, 64<<10)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || string(bufA[:na]) != string(bufB[:nb]) {
			return false, nil
		}
		endA := errA == io.EOF || errA == io.ErrUnexpectedEOF
		endB := errB == io.EOF || errB == io.ErrUnexpectedEOF
		if errA != nil && !endA {
			return false, errA
		}
		if errB != nil && !endB {
			return false, errB
		}
		if endA || endB {
			return endA && endB, nil
		}
	}
}

// mergeCandidateFiles unions model-reported FILE blocks with the clone diff.
// The clone diff is authoritative: when both describe the same path, the clone
// content wins because it is what verification actually observed on disk.
func mergeCandidateFiles(reported, cloneDiff []ExtractedFile) []ExtractedFile {
	if len(cloneDiff) == 0 {
		return reported
	}
	merged := make(map[string]ExtractedFile, len(reported)+len(cloneDiff))
	for _, f := range reported {
		merged[f.Path] = f
	}
	for _, f := range cloneDiff {
		merged[f.Path] = f
	}

	out := make([]ExtractedFile, 0, len(merged))
	for _, file := range merged {
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// extractedFilesEqual compares two file sets by path and content, ignoring
// order and duplicate paths (last occurrence wins, matching write semantics).
func extractedFilesEqual(a, b []ExtractedFile) bool {
	toMap := func(files []ExtractedFile) map[string]ExtractedFile {
		m := make(map[string]ExtractedFile, len(files))
		for _, f := range files {
			m[f.Path] = f
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
	contract, err := p.baselineVerificationContract()
	if err != nil {
		return CandidateVerification{TestsError: err.Error()}
	}
	before, err := workspaceInputDigest(p.Path)
	if err != nil {
		return CandidateVerification{IntegrityError: err.Error()}
	}
	report := p.verifyRestrictedWorkspace(ctx, files, contract)
	after, err := workspaceInputDigest(p.Path)
	if err != nil || before != after {
		report.Passed = false
		report.Strength = 0
		report.IntegrityError = "verification changed its input files or permissions; verify the new state again"
	}
	return report
}

func (p *Project) verifyRestrictedWorkspace(ctx context.Context, files []ExtractedFile, contract verificationContract) CandidateVerification {
	report := CandidateVerification{BaselineTests: contract.hasTests}

	if !p.runInlineQuickChecks(files, &report) {
		return report
	}

	// Fail closed: everything below executes candidate-influenced commands, so
	// without working sandbox isolation (or the acknowledged unsafe host
	// opt-in) nothing runs and the candidate stays unverified.
	execEnv, isoErr := resolveVerifyExecEnvironment(p.unsafeHostAuth)
	if isoErr != nil {
		report.IsolationError = isoErr.Error()
		return report
	}
	report.Isolated = execEnv.mode == verifyExecIsolated

	if !p.runExternalQuickChecks(ctx, files, &report) {
		return report
	}

	depsPlan := contract.deps
	if depsPlan != nil {
		report.DepsPlan = depsPlan
	}
	if depsPlan != nil && shouldRunDependencyPlan(*depsPlan, files) {
		result, err := p.RunVerificationPlan(ctx, *depsPlan)
		if result != nil {
			report.DepsResult = result
		}
		if err != nil {
			report.DepsError = err.Error()
			report.EnvironmentError = isEnvironmentExecError(err)
			return report
		}
		if result != nil && result.ExitCode != 0 {
			report.DepsError = execFailureDetail(result)
			return report
		}
	} else if depsPlan != nil {
		report.DepsSkipped = true
	}

	testsPlan := contract.tests
	if testsPlan != nil {
		report.TestsPlan = testsPlan
		report.HasTests = true
	}
	if testsPlan != nil {
		runPlan := *testsPlan
		if runPlan.Command == "go" && len(runPlan.Args) > 0 && runPlan.Args[0] == "test" {
			// Disable cached results and require the tool's structured event stream.
			runPlan.Args = append([]string{"test", "-json", "-count=1"}, runPlan.Args[1:]...)
		}

		result, err := p.RunVerificationPlan(ctx, runPlan)
		if result != nil {
			report.TestsResult = result
		}
		if err != nil {
			report.TestsError = err.Error()
			report.EnvironmentError = isEnvironmentExecError(err)
			return report
		}
		if result != nil && result.ExitCode != 0 {
			report.TestsError = execFailureDetail(result)
			return report
		}
		report.Passed = true
		report.NoTestsRan = detectNoTestsRun(runPlan, result)
		if runPlan.Command == "go" && len(contract.goTests) > 0 && !goBaselineTestsPassed(contract.goTests, result) {
			report.Passed = false
			report.NoTestsRan = true
			report.TestsError = "test output did not confirm every expected baseline Go test"
			return report
		}
		// Test code and the implementation share a process. Even structured output
		// can be forged by that process; it cannot authorize automatic application.
		// Strength 2 is reserved for an independent trusted acceptance driver.
		report.Strength = 1
		report.EvidenceLimit = "local checks passed; candidate-controlled test output requires human approval"

		return report
	}

	// No baseline test plan: only syntax/compile quick checks ran. That is not
	// verification, so the candidate is reported as unverified (never Passed)
	// and callers present it as "no tests were executed".
	report.NoTestPlan = true
	report.NoTestsRan = true
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
		return !goTestOutputRanTests(plan, result.Stdout)
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

func goTestOutputRanTests(plan ExecPlan, stdout string) bool {
	if !slices.Contains(plan.Args, "-json") {
		return false
	}
	for _, event := range parseGoTestEvents(stdout) {
		if event.Action == "pass" && event.Test != "" {
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
			report.EnvironmentError = isEnvironmentExecError(err)
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

// isEnvironmentExecError reports whether a RunVerificationPlan error means
// the check could not run (sandbox setup, host toolchain, ...) rather than the
// candidate misbehaving. Touching protected paths is the candidate's doing.
func isEnvironmentExecError(err error) bool {
	return err != nil && !errors.Is(err, ErrProtectedPathsModified)
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
