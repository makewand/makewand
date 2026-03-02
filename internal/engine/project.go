package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Project represents a makewand project.
type Project struct {
	Name     string
	Path     string
	Files    []FileEntry
}

// FileEntry represents a file in the project tree.
type FileEntry struct {
	Path     string
	IsDir    bool
	Size     int64
	Modified bool // changed in current session
}

// NewProject creates a new project in the given directory.
func NewProject(name, parentDir string) (*Project, error) {
	// Sanitize name for directory
	safeName := sanitizeDirName(name)
	path := filepath.Join(parentDir, safeName)

	if err := os.MkdirAll(path, 0755); err != nil {
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

	p := &Project{
		Name: filepath.Base(path),
		Path: path,
	}

	if err := p.ScanFiles(); err != nil {
		return nil, err
	}

	return p, nil
}

// ScanFiles scans the project directory and populates the file list.
func (p *Project) ScanFiles() error {
	p.Files = nil

	return filepath.Walk(p.Path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}

		// Skip hidden directories and common ignores
		rel, _ := filepath.Rel(p.Path, path)
		if shouldIgnore(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		p.Files = append(p.Files, FileEntry{
			Path:  rel,
			IsDir: info.IsDir(),
			Size:  info.Size(),
		})

		return nil
	})
}

// WriteFile writes content to a file in the project.
func (p *Project) WriteFile(relPath, content string) error {
	fullPath := filepath.Join(p.Path, relPath)

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

// ReadFile reads a file from the project.
func (p *Project) ReadFile(relPath string) (string, error) {
	fullPath := filepath.Join(p.Path, relPath)
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
	// Replace spaces and special chars with hyphens
	name = strings.ToLower(name)
	replacer := strings.NewReplacer(
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
	return replacer.Replace(name)
}

func shouldIgnore(path string) bool {
	ignores := []string{
		".git",
		"node_modules",
		"__pycache__",
		".venv",
		"venv",
		".DS_Store",
		".makewand",
	}
	base := filepath.Base(path)
	for _, ig := range ignores {
		if base == ig {
			return true
		}
	}
	return false
}
