package engine

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Project represents a makewand project.
type Project struct {
	Name  string
	Path  string
	Files []FileEntry
}

// FileEntry represents a file in the project tree.
type FileEntry struct {
	Path     string
	IsDir    bool
	Size     int64
	Modified bool // changed in current session
}

// shouldIgnoreSet is a package-level set for fast ignore lookups.
var shouldIgnoreSet = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".DS_Store":    true,
	".makewand":    true,
}

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

// NewProject creates a new project in the given directory.
func NewProject(name, parentDir string) (*Project, error) {
	safeName := sanitizeDirName(name)
	path := filepath.Join(parentDir, safeName)

	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, fmt.Errorf("create project directory: %w", err)
	}

	return &Project{
		Name: name,
		Path: path,
	}, nil
}

// OpenProject opens an existing project from the current directory.
func OpenProject(path string) (*Project, error) {
	info, err := os.Stat(path)
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

	if err := p.ScanFiles(); err != nil {
		return nil, err
	}

	return p, nil
}

// ScanFiles scans the project directory and populates the file list.
func (p *Project) ScanFiles() error {
	p.Files = nil

	return filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, err error) error {
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

		return nil
	})
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
	fullPath, err := p.validatePath(relPath, true)
	if err != nil {
		return err
	}

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Re-validate after directory creation to reduce TOCTOU exposure.
	if _, err := p.validatePath(relPath, true); err != nil {
		return err
	}

	if err := os.WriteFile(fullPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

const maxReadFileSize = 10 << 20 // 10 MB

// ReadFile reads a file from the project.
func (p *Project) ReadFile(relPath string) (string, error) {
	fullPath, err := p.validatePath(relPath, false)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if info.Size() > maxReadFileSize {
		return "", fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxReadFileSize)
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
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
	return shouldIgnoreSet[base]
}
