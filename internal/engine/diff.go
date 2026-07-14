package engine

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// EditBlock represents a search/replace operation on a file.
type EditBlock struct {
	Path    string
	Search  string
	Replace string
}

// DiffBlock represents a unified diff to apply to a file.
type DiffBlock struct {
	Path  string
	Patch string
}

// editHeaderRe matches "--- EDIT: path/to/file ---".
var editHeaderRe = regexp.MustCompile(`^---\s*EDIT:\s*(.+?)\s*---\s*$`)

// diffHeaderRe matches "--- DIFF: path/to/file ---".
var diffHeaderRe = regexp.MustCompile(`^---\s*DIFF:\s*(.+?)\s*---\s*$`)

// Multiline versions for quick scanning.
var editHeaderReMulti = regexp.MustCompile(`(?m)^---\s*EDIT:\s*.+?\s*---\s*$`)
var diffHeaderReMulti = regexp.MustCompile(`(?m)^---\s*DIFF:\s*.+?\s*---\s*$`)

// ParseEdits extracts EDIT blocks from AI output text.
//
// Format:
//
//	--- EDIT: path/to/file ---
//	<<<< SEARCH
//	old code here
//	====
//	new code here
//	>>>> REPLACE
func ParseEdits(text string) []EditBlock {
	lines := strings.Split(text, "\n")
	var edits []EditBlock

	i := 0
	for i < len(lines) {
		m := editHeaderRe.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			i++
			continue
		}
		path := strings.TrimSpace(m[1])
		i++

		// There may be multiple search/replace pairs under one header.
		for i < len(lines) {
			// Skip blank lines between pairs.
			for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			if i >= len(lines) {
				break
			}

			// Check for <<<< SEARCH marker.
			if strings.TrimSpace(lines[i]) != "<<<< SEARCH" {
				break
			}
			i++

			// Collect search text until ====.
			var searchBuf strings.Builder
			for i < len(lines) && strings.TrimSpace(lines[i]) != "====" {
				if searchBuf.Len() > 0 {
					searchBuf.WriteString("\n")
				}
				searchBuf.WriteString(lines[i])
				i++
			}
			if i >= len(lines) {
				break
			}
			i++ // skip ====

			// Collect replace text until >>>> REPLACE.
			var replaceBuf strings.Builder
			for i < len(lines) && strings.TrimSpace(lines[i]) != ">>>> REPLACE" {
				if replaceBuf.Len() > 0 {
					replaceBuf.WriteString("\n")
				}
				replaceBuf.WriteString(lines[i])
				i++
			}
			if i < len(lines) {
				i++ // skip >>>> REPLACE
			}

			edits = append(edits, EditBlock{
				Path:    path,
				Search:  searchBuf.String(),
				Replace: replaceBuf.String(),
			})
		}
	}

	return edits
}

// ParseDiffs extracts DIFF blocks from AI output text.
//
// Format:
//
//	--- DIFF: path/to/file ---
//	@@ -10,5 +10,6 @@
//	 context line
//	-old line
//	+new line
//	 context line
func ParseDiffs(text string) []DiffBlock {
	lines := strings.Split(text, "\n")
	var diffs []DiffBlock

	i := 0
	for i < len(lines) {
		m := diffHeaderRe.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			i++
			continue
		}
		path := strings.TrimSpace(m[1])
		i++

		// Collect everything until the next header or end of text.
		var patchBuf strings.Builder
		for i < len(lines) {
			// Stop if we see another DIFF or EDIT or FILE header.
			trimmed := strings.TrimSpace(lines[i])
			if diffHeaderRe.MatchString(trimmed) || editHeaderRe.MatchString(trimmed) || fileHeaderRe.MatchString(trimmed) {
				break
			}
			if patchBuf.Len() > 0 {
				patchBuf.WriteString("\n")
			}
			patchBuf.WriteString(lines[i])
			i++
		}

		patch := strings.TrimRight(patchBuf.String(), "\n ")
		if patch != "" {
			diffs = append(diffs, DiffBlock{
				Path:  path,
				Patch: patch,
			})
		}
	}

	return diffs
}

// ApplyEdit applies a search/replace edit to a file on disk.
// It reads the file, finds the first occurrence of edit.Search,
// replaces it with edit.Replace, and writes the file back.
func ApplyEdit(filePath string, edit EditBlock) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file %s: %w", filePath, err)
	}

	content := string(data)
	idx := strings.Index(content, edit.Search)
	if idx < 0 {
		return fmt.Errorf("search text not found in %s", filePath)
	}

	// Replace only the first occurrence.
	newContent := content[:idx] + edit.Replace + content[idx+len(edit.Search):]

	return os.WriteFile(filePath, []byte(newContent), 0644)
}

// hunk represents a single @@ hunk in a unified diff.
type hunk struct {
	oldStart int // 1-based line number in original file
	oldCount int
	newStart int // 1-based line number in new file
	newCount int
	lines    []diffLine
}

type diffLine struct {
	op   byte   // ' ', '+', '-'
	text string // line content without the prefix
}

var hunkHeaderRe = regexp.MustCompile(`^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@`)

// parseHunks parses unified diff hunks from patch text.
func parseHunks(patch string) ([]hunk, error) {
	lines := strings.Split(patch, "\n")
	var hunks []hunk

	i := 0
	for i < len(lines) {
		m := hunkHeaderRe.FindStringSubmatch(lines[i])
		if m == nil {
			i++
			continue
		}

		oldStart, _ := strconv.Atoi(m[1])
		oldCount := 1
		if m[2] != "" {
			oldCount, _ = strconv.Atoi(m[2])
		}
		newStart, _ := strconv.Atoi(m[3])
		newCount := 1
		if m[4] != "" {
			newCount, _ = strconv.Atoi(m[4])
		}

		h := hunk{
			oldStart: oldStart,
			oldCount: oldCount,
			newStart: newStart,
			newCount: newCount,
		}
		i++

		// Parse diff lines until next hunk header or end.
		for i < len(lines) {
			if hunkHeaderRe.MatchString(lines[i]) {
				break
			}
			line := lines[i]
			if len(line) == 0 {
				// Empty line in diff = context line with empty content.
				h.lines = append(h.lines, diffLine{op: ' ', text: ""})
			} else {
				op := line[0]
				rest := line[1:]
				switch op {
				case ' ', '+', '-':
					h.lines = append(h.lines, diffLine{op: op, text: rest})
				default:
					// Treat as context line (some diffs omit the space prefix).
					h.lines = append(h.lines, diffLine{op: ' ', text: line})
				}
			}
			i++
		}

		hunks = append(hunks, h)
	}

	if len(hunks) == 0 {
		return nil, fmt.Errorf("no valid hunks found in diff")
	}

	return hunks, nil
}

// ApplyDiff applies a unified diff to a file on disk.
// It parses the @@ hunks and applies them line by line.
// Context lines must match the file content or an error is returned.
func ApplyDiff(filePath string, diff DiffBlock) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file %s: %w", filePath, err)
	}

	hunks, err := parseHunks(diff.Patch)
	if err != nil {
		return fmt.Errorf("parse diff for %s: %w", filePath, err)
	}

	fileLines := strings.Split(string(data), "\n")

	// Apply hunks in reverse order so earlier line numbers stay valid.
	for i := len(hunks) - 1; i >= 0; i-- {
		h := hunks[i]
		// Convert to 0-based index.
		pos := h.oldStart - 1
		if pos < 0 {
			pos = 0
		}

		// Separate old (context + removed) and new (context + added) lines.
		var oldLines []string
		var newLines []string
		for _, dl := range h.lines {
			switch dl.op {
			case ' ':
				oldLines = append(oldLines, dl.text)
				newLines = append(newLines, dl.text)
			case '-':
				oldLines = append(oldLines, dl.text)
			case '+':
				newLines = append(newLines, dl.text)
			}
		}

		// Verify context: oldLines must match file content at pos.
		if pos+len(oldLines) > len(fileLines) {
			return fmt.Errorf("hunk @@ -%d,%d out of range (file has %d lines)",
				h.oldStart, h.oldCount, len(fileLines))
		}
		for j, ol := range oldLines {
			if fileLines[pos+j] != ol {
				return fmt.Errorf("context mismatch at line %d: expected %q, got %q",
					pos+j+1, ol, fileLines[pos+j])
			}
		}

		// Splice: remove old lines, insert new lines.
		result := make([]string, 0, len(fileLines)-len(oldLines)+len(newLines))
		result = append(result, fileLines[:pos]...)
		result = append(result, newLines...)
		result = append(result, fileLines[pos+len(oldLines):]...)
		fileLines = result
	}

	return os.WriteFile(filePath, []byte(strings.Join(fileLines, "\n")), 0644)
}

// ContainsEdits does a quick check whether text likely contains EDIT or DIFF blocks.
func ContainsEdits(text string) bool {
	return editHeaderReMulti.MatchString(text) || diffHeaderReMulti.MatchString(text)
}
