package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Project represents a makewand project.
type Project struct {
	Name          string
	Path          string
	Files         []FileEntry
	ScanTruncated bool

	// unsafeHostAuth is the app-layer-resolved authorization for the
	// MAKEWAND_UNSAFE_HOST_EXEC opt-in. Zero value = not acknowledged, so
	// restricted plans fail closed instead of running on the host. Set via
	// SetUnsafeHostExecAuthorization; CloneToTemp propagates it.
	unsafeHostAuth UnsafeHostExecAuthorization
}

// FileEntry represents a file in the project tree.
type FileEntry struct {
	Path     string
	IsDir    bool
	Size     int64
	Modified bool // changed in current session
}

// shouldIgnoreSet is an alias to ProjectIgnoreDirs for backwards compatibility.
var shouldIgnoreSet = ProjectIgnoreDirs

// sanitizeReplacer is a package-level replacer for directory name sanitization.
var sanitizeReplacer = strings.NewReplacer(
	" ", "-",
	"/", "-",
	"\\", "-",
	":", "-",
	"*", "",
	"?", "",
	"\"", "",
	"<", "",
	">", "",
	"|", "",
)

var errProjectScanLimitReached = errors.New("project scan limit reached")

// protectedWriteDirs are directories that AI-generated file writes must never
// touch: repository metadata (git hooks would execute on the host) and CI/CD
// definitions (they run in trusted contexts and gate verification).
var protectedWriteDirs = map[string]bool{
	".git":       true,
	".github":    true,
	".circleci":  true,
	".buildkite": true,
}

// protectedWriteFiles are root-level verification/CI entry points that
// AI-generated file writes must never replace. Makefile and the shell scripts
// under scripts/ (handled separately) gate verification, so a candidate must
// not be able to weaken the very scripts that judge it.
var protectedWriteFiles = map[string]bool{
	".gitlab-ci.yml":      true,
	".travis.yml":         true,
	"Jenkinsfile":         true,
	"azure-pipelines.yml": true,
	"Makefile":            true,
	"makefile":            true,
	"GNUmakefile":         true,
}

// isProtectedWritePath reports whether a cleaned project-relative path is
// write-protected against generated content. Applies to both verification
// clones and real project applies.
func isProtectedWritePath(cleaned string) bool {
	slashed := filepath.ToSlash(cleaned)
	parts := strings.Split(slashed, "/")
	// .git is protected at any depth: hooks in nested repositories execute on
	// the host too.
	for _, part := range parts {
		if part == ".git" {
			return true
		}
	}
	if protectedWriteDirs[parts[0]] {
		return true
	}
	// Shell scripts under scripts/ are the project's CI/verification entry
	// points (test_gate.sh, test_race.sh, ...). Protect them without over-
	// blocking non-shell project source that may also live under scripts/.
	if len(parts) > 1 && parts[0] == "scripts" && strings.HasSuffix(strings.ToLower(slashed), ".sh") {
		return true
	}
	// .makewand/rules.md is injected into every trusted-mode system prompt as
	// "Project rules"; generated content (and code run by restricted plans)
	// must never be able to plant persistent instructions there. Compared
	// case-insensitively for case-insensitive filesystems.
	if strings.EqualFold(slashed, protectedRulesPath) {
		return true
	}
	return protectedWriteFiles[slashed]
}

// protectedRulesPath is the trusted project-rules file loaded by LoadRepoContext.
const protectedRulesPath = ".makewand/rules.md"

// NewProject creates a new project in the given directory.
func NewProject(name, parentDir string) (*Project, error) {
	safeName := sanitizeDirName(name)
	path := filepath.Join(parentDir, safeName)

	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, fmt.Errorf("create project directory: %w", err)
	}

	p := &Project{
		Name: name,
		Path: path,
	}
	if err := p.recoverOnOpen(); err != nil {
		return nil, err
	}
	return p, nil
}

// OpenProject opens an existing project from the current directory.
func OpenProject(path string) (*Project, error) {
	return openProject(path, 0)
}

// OpenProjectLimited opens an existing project but caps the number of scanned
// entries. This is useful for latency-sensitive paths such as headless mode.
func OpenProjectLimited(path string, maxEntries int) (*Project, error) {
	return openProject(path, maxEntries)
}

func openProject(path string, maxEntries int) (*Project, error) {
	info, err := os.Stat(path) //nolint:gosec // Read-only metadata for the user-selected project directory; recovery binds the opened root identity.
	if err != nil {
		return nil, fmt.Errorf("open project: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", path)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	p := &Project{
		Name: filepath.Base(absPath),
		Path: absPath,
	}
	if err := p.recoverOnOpen(); err != nil {
		return nil, err
	}

	if err := p.scanFiles(maxEntries); err != nil {
		return nil, err
	}

	return p, nil
}

func (p *Project) recoverOnOpen() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.RecoverInterruptedApply(ctx); err != nil {
		return fmt.Errorf("recover interrupted project apply: %w", err)
	}
	return nil
}

// ScanFiles scans the project directory and populates the file list.
func (p *Project) ScanFiles() error {
	return p.scanFiles(0)
}

// ScanFilesLimited scans the project directory and stops after maxEntries
// non-root entries. A non-positive limit means unlimited scanning.
func (p *Project) ScanFilesLimited(maxEntries int) error {
	return p.scanFiles(maxEntries)
}

func (p *Project) scanFiles(maxEntries int) error {
	p.Files = nil
	p.ScanTruncated = false
	count := 0

	err := filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == p.Path {
				return err
			}
			return nil // skip errors
		}

		rel, _ := filepath.Rel(p.Path, path)
		if shouldIgnore(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel != "." && maxEntries > 0 && count >= maxEntries {
			p.ScanTruncated = true
			return errProjectScanLimitReached
		}

		var size int64
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				size = info.Size()
			}
		}

		p.Files = append(p.Files, FileEntry{
			Path:  rel,
			IsDir: d.IsDir(),
			Size:  size,
		})
		if rel != "." {
			count++
		}

		return nil
	})
	if err != nil && !errors.Is(err, errProjectScanLimitReached) {
		return err
	}
	return nil
}

// validatePath checks that a relative path resolves to within the project directory.
// It rejects absolute paths, traversal, and symlink escapes.
func (p *Project) validatePath(relPath string, forWrite bool) (string, error) {
	cleaned := filepath.Clean(relPath)
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("invalid path: %s", relPath)
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths not allowed: %s", relPath)
	}
	if forWrite && isProtectedWritePath(cleaned) {
		return "", fmt.Errorf("write to protected path not allowed: %s", relPath)
	}

	projectAbs, err := filepath.Abs(p.Path)
	if err != nil {
		return "", fmt.Errorf("resolve project path: %w", err)
	}

	fullPath := filepath.Join(projectAbs, cleaned)
	if !isWithinDir(projectAbs, fullPath) {
		return "", fmt.Errorf("path traversal not allowed: %s", relPath)
	}

	relFromProject, err := filepath.Rel(projectAbs, fullPath)
	if err != nil {
		return "", fmt.Errorf("resolve relative path: %w", err)
	}
	if err := validateNoSymlinkEscape(projectAbs, relFromProject, forWrite); err != nil {
		return "", err
	}

	// Avoid following a symlink at the final write target.
	if forWrite {
		info, err := os.Lstat(fullPath)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to write through symlink: %s", relPath)
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect target path: %w", err)
		}
	}

	return fullPath, nil
}

func validateNoSymlinkEscape(projectAbs, relPath string, forWrite bool) error {
	parts := strings.Split(relPath, string(filepath.Separator))
	limit := len(parts)
	if forWrite && limit > 0 {
		// For writes, the final element can be a new file; validate parent chain.
		limit--
	}

	current := projectAbs
	for i := 0; i < limit; i++ {
		part := parts[i]
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)

		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect path: %w", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}

		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return fmt.Errorf("resolve symlink: %w", err)
		}
		if !isWithinDir(projectAbs, resolved) {
			return fmt.Errorf("path traversal not allowed via symlink: %s", relPath)
		}
	}

	return nil
}

func isWithinDir(base, target string) bool {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}

	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// WriteFile writes content to a file in the project.
func (p *Project) WriteFile(relPath, content string) error {
	return p.writeFileMode(relPath, content, nil)
}

func (p *Project) writeFileMode(relPath, content string, requestedMode *fs.FileMode) error {
	if _, err := p.validatePath(relPath, true); err != nil {
		return err
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeFileModeRoot(root, relPath, content, requestedMode)
}

func writeFileModeRoot(root *os.Root, relPath, content string, requestedMode *fs.FileMode) error {
	tempPath := filepath.Join(filepath.Dir(filepath.Clean(relPath)), ".makewand-write-"+rand.Text())
	return writeFileModeAt(root, relPath, content, requestedMode, tempPath, "")
}

func writeFileModeAt(root *os.Root, relPath, content string, requestedMode *fs.FileMode, tempPath, security string, beforeRename ...func(*os.File) error) error {
	relPath = filepath.Clean(relPath)
	mode := fs.FileMode(0o600)
	if info, err := root.Lstat(relPath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular file: %s", relPath)
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	if requestedMode != nil {
		mode = requestedMode.Perm()
	}
	dir := filepath.Dir(relPath)
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	// Root-relative operations resist parent symlink swaps; atomic replacement
	// preserves the old file on errors and never truncates shared hardlink inodes.
	tmp, err := root.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() { _ = root.Remove(tempPath) }()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := applySetOpenFileSecurity(tmp, security); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	for _, prepare := range beforeRename {
		if prepare != nil {
			if err := prepare(tmp); err != nil {
				tmp.Close()
				return err
			}
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceApplyFile(root, tempPath, relPath); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}

const maxReadFileSize = 10 << 20 // 10 MB

// ReadFile reads a file from the project using Go 1.24 os.OpenRoot to prevent TOCTOU escapes.
func (p *Project) ReadFile(relPath string) (string, error) {
	if _, err := p.validatePath(relPath, false); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	defer root.Close()

	cleanRel := filepath.Clean(relPath)
	info, err := root.Stat(cleanRel)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read file: %s is a directory", relPath)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read file: %s is not a regular file", relPath)
	}
	if info.Size() > maxReadFileSize {
		return "", fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxReadFileSize)
	}

	f, err := root.Open(cleanRel)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	defer f.Close()

	openInfo, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if !openInfo.Mode().IsRegular() {
		return "", fmt.Errorf("read file: %s is not a regular file", relPath)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxReadFileSize+1))
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if int64(len(data)) > maxReadFileSize {
		return "", fmt.Errorf("file too large: %d bytes (max %d)", len(data), maxReadFileSize)
	}
	return string(data), nil
}

// FileTree returns a formatted tree string of the project files.
func (p *Project) FileTree() string {
	if len(p.Files) == 0 {
		return "(empty project)"
	}

	var b strings.Builder
	for i, f := range p.Files {
		if f.Path == "." {
			continue
		}

		depth := strings.Count(f.Path, string(filepath.Separator))
		prefix := strings.Repeat("  ", depth)

		isLast := i == len(p.Files)-1
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		name := filepath.Base(f.Path)
		if f.IsDir {
			name += "/"
		}

		b.WriteString(prefix + connector + name + "\n")
	}

	return b.String()
}

func sanitizeDirName(name string) string {
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.ToLower(name)
	name = sanitizeReplacer.Replace(name)

	// Strip leading dots and hyphens
	name = strings.TrimLeft(name, ".-")

	// Limit length
	if len(name) > 128 {
		name = name[:128]
	}

	if name == "" {
		name = "project"
	}

	return name
}

func shouldIgnore(path string) bool {
	base := filepath.Base(path)
	if shouldIgnoreSet[base] {
		return true
	}
	return isWorkspaceSecretPath(path)
}
