package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RepoContext holds project-level context for AI prompts.
type RepoContext struct {
	Rules     string            // content of .makewand/rules.md
	FileHints map[string]string // path → first N lines (for key files)
	Symbols   []Symbol          // extracted symbols from Go/JS/TS/Python files
}

// Symbol represents a code symbol extracted from source files.
type Symbol struct {
	Name string // e.g. "Router", "ParseFiles"
	Kind string // "func", "type", "class", "const"
	File string // relative file path
	Line int    // line number
}

// keyFileNames lists filenames whose first few lines are included as hints.
var keyFileNames = map[string]bool{
	"main.go":       true,
	"main.py":       true,
	"main.ts":       true,
	"main.js":       true,
	"app.go":        true,
	"app.py":        true,
	"app.ts":        true,
	"app.js":        true,
	"index.ts":      true,
	"index.js":      true,
	"package.json":  true,
	"go.mod":        true,
	"Makefile":      true,
	"Cargo.toml":    true,
	"pyproject.toml": true,
	"requirements.txt": true,
	"Dockerfile":    true,
}

const fileHintLines = 5

// Symbol extraction regexes by language family.
var (
	goFuncRe   = regexp.MustCompile(`^func\s+(\w+)\s*\(`)
	goMethodRe = regexp.MustCompile(`^func\s+\([^)]+\)\s+(\w+)\s*\(`)
	goTypeRe   = regexp.MustCompile(`^type\s+(\w+)\s+`)

	jsFuncRe    = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?function\s+(\w+)`)
	jsClassRe   = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?class\s+(\w+)`)
	jsConstRe   = regexp.MustCompile(`^export\s+(?:default\s+)?(?:const|let)\s+(\w+)`)

	pyDefRe   = regexp.MustCompile(`^\s*def\s+(\w+)\s*\(`)
	pyClassRe = regexp.MustCompile(`^\s*class\s+(\w+)[\s(:]`)
)

// symbolExtractable returns the language family for symbol extraction, or "".
func symbolExtractable(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".js", ".ts", ".jsx", ".tsx", ".mjs":
		return "js"
	case ".py":
		return "python"
	default:
		return ""
	}
}

// LoadRepoContext loads project rules, file hints, and symbols.
func LoadRepoContext(projectDir string, files []FileEntry) (*RepoContext, error) {
	rc := &RepoContext{
		FileHints: make(map[string]string),
	}

	// Load rules from .makewand/rules.md (optional).
	rulesPath := filepath.Join(projectDir, ".makewand", "rules.md")
	if data, err := os.ReadFile(rulesPath); err == nil {
		rc.Rules = strings.TrimSpace(string(data))
	}

	// Extract file hints for key files.
	for _, f := range files {
		if f.IsDir {
			continue
		}
		base := filepath.Base(f.Path)
		if !keyFileNames[base] {
			continue
		}
		if hint := readFirstLines(filepath.Join(projectDir, f.Path), fileHintLines); hint != "" {
			rc.FileHints[f.Path] = hint
		}
	}

	// Extract symbols.
	rc.Symbols = ExtractSymbols(projectDir, files)

	return rc, nil
}

// readFirstLines reads the first n lines from a file, returning them joined.
func readFirstLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for i := 0; i < n && scanner.Scan(); i++ {
		lines = append(lines, scanner.Text())
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// ExtractSymbols performs simple regex-based symbol extraction from source files.
func ExtractSymbols(projectDir string, files []FileEntry) []Symbol {
	var symbols []Symbol
	for _, f := range files {
		if f.IsDir {
			continue
		}
		lang := symbolExtractable(f.Path)
		if lang == "" {
			continue
		}
		syms := extractFileSymbols(filepath.Join(projectDir, f.Path), f.Path, lang)
		symbols = append(symbols, syms...)
	}
	return symbols
}

func extractFileSymbols(fullPath, relPath, lang string) []Symbol {
	file, err := os.Open(fullPath)
	if err != nil {
		return nil
	}
	defer file.Close()

	var symbols []Symbol
	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		switch lang {
		case "go":
			// Check method before func (method regex is more specific).
			if m := goMethodRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "func", File: relPath, Line: lineNum})
			} else if m := goFuncRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "func", File: relPath, Line: lineNum})
			} else if m := goTypeRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "type", File: relPath, Line: lineNum})
			}
		case "js":
			if m := jsClassRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "class", File: relPath, Line: lineNum})
			} else if m := jsFuncRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "func", File: relPath, Line: lineNum})
			} else if m := jsConstRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "const", File: relPath, Line: lineNum})
			}
		case "python":
			if m := pyClassRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "class", File: relPath, Line: lineNum})
			} else if m := pyDefRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Name: m[1], Kind: "func", File: relPath, Line: lineNum})
			}
		}
	}
	return symbols
}

// ForPrompt formats the repo context for inclusion in a system prompt,
// respecting a character budget. Priority: rules first, then symbols, then file hints.
func (rc *RepoContext) ForPrompt(maxChars int) string {
	if rc == nil || maxChars <= 0 {
		return ""
	}

	var b strings.Builder

	// 1. Rules (highest priority).
	if rc.Rules != "" {
		section := "\nProject rules:\n" + rc.Rules + "\n"
		if b.Len()+len(section) <= maxChars {
			b.WriteString(section)
		} else {
			// Truncate rules to fit.
			remaining := maxChars - b.Len() - len("\nProject rules:\n") - len("\n...[truncated]\n")
			if remaining > 50 {
				b.WriteString("\nProject rules:\n")
				b.WriteString(rc.Rules[:remaining])
				b.WriteString("\n...[truncated]\n")
			}
		}
	}

	// 2. Symbols summary.
	if len(rc.Symbols) > 0 && b.Len() < maxChars {
		var symLines []string
		for _, s := range rc.Symbols {
			symLines = append(symLines, fmt.Sprintf("  %s %s (%s:%d)", s.Kind, s.Name, s.File, s.Line))
		}
		section := "\nKey symbols:\n" + strings.Join(symLines, "\n") + "\n"
		if b.Len()+len(section) <= maxChars {
			b.WriteString(section)
		} else {
			// Include as many symbol lines as fit.
			budget := maxChars - b.Len() - len("\nKey symbols:\n") - len("\n")
			if budget > 30 {
				b.WriteString("\nKey symbols:\n")
				used := 0
				for _, line := range symLines {
					if used+len(line)+1 > budget {
						break
					}
					b.WriteString(line + "\n")
					used += len(line) + 1
				}
			}
		}
	}

	// 3. File hints.
	if len(rc.FileHints) > 0 && b.Len() < maxChars {
		var hintParts []string
		for path, hint := range rc.FileHints {
			hintParts = append(hintParts, fmt.Sprintf("  %s:\n    %s", path, strings.ReplaceAll(hint, "\n", "\n    ")))
		}
		section := "\nFile hints:\n" + strings.Join(hintParts, "\n") + "\n"
		if b.Len()+len(section) <= maxChars {
			b.WriteString(section)
		} else {
			budget := maxChars - b.Len() - len("\nFile hints:\n") - len("\n")
			if budget > 30 {
				b.WriteString("\nFile hints:\n")
				used := 0
				for _, part := range hintParts {
					if used+len(part)+1 > budget {
						break
					}
					b.WriteString(part + "\n")
					used += len(part) + 1
				}
			}
		}
	}

	return b.String()
}
