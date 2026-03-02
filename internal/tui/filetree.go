package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
)

// FileTreePanel displays the project file structure.
type FileTreePanel struct {
	files  []engine.FileEntry
	width  int
	height int
}

// NewFileTreePanel creates a new file tree panel.
func NewFileTreePanel() FileTreePanel {
	return FileTreePanel{}
}

// SetFiles updates the file list.
func (f *FileTreePanel) SetFiles(files []engine.FileEntry) {
	f.files = files
}

// SetSize sets the panel dimensions.
func (f *FileTreePanel) SetSize(width, height int) {
	f.width = width
	f.height = height
}

// View renders the file tree.
func (f FileTreePanel) View() string {
	msg := i18n.Msg()
	title := titleStyle.Render("📁 " + msg.FileTreeTitle)

	if len(f.files) == 0 {
		return fileBorderStyle.Width(f.width - 2).Render(
			title + "\n" + mutedStyle.Render("  (no files yet)"),
		)
	}

	var b strings.Builder
	b.WriteString(title + "\n")

	// Build tree structure
	maxLines := f.height - 4
	if maxLines < 3 {
		maxLines = 3
	}

	count := 0
	for _, file := range f.files {
		if file.Path == "." {
			continue
		}
		if count >= maxLines {
			remaining := len(f.files) - count
			b.WriteString(mutedStyle.Render(fmt.Sprintf("  ... +%d more files", remaining)))
			break
		}

		depth := strings.Count(file.Path, string(filepath.Separator))
		indent := strings.Repeat("  ", depth)
		name := filepath.Base(file.Path)

		icon := fileIcon(name, file.IsDir)
		if file.IsDir {
			name += "/"
		}

		line := fmt.Sprintf("%s%s %s", indent, icon, name)

		if file.Modified {
			line = successStyle.Render(line)
		}

		b.WriteString(line + "\n")
		count++
	}

	return fileBorderStyle.Width(f.width - 2).Render(b.String())
}

func fileIcon(name string, isDir bool) string {
	if isDir {
		return "📂"
	}

	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".go":
		return "🔵"
	case ".py":
		return "🐍"
	case ".js", ".jsx", ".ts", ".tsx":
		return "🟨"
	case ".html":
		return "🌐"
	case ".css", ".scss":
		return "🎨"
	case ".json":
		return "📋"
	case ".yaml", ".yml":
		return "⚙️"
	case ".md":
		return "📝"
	case ".sql":
		return "🗄️"
	case ".sh", ".bash":
		return "🐚"
	case ".toml":
		return "⚙️"
	case ".mod", ".sum":
		return "📦"
	case ".gitignore":
		return "🙈"
	default:
		return "📄"
	}
}
