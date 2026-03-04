package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ExtractedFile represents a file parsed from AI output.
type ExtractedFile struct {
	Path    string
	Content string
}

// ParseResult holds the result of parsing AI output for file blocks.
type ParseResult struct {
	Files       []ExtractedFile
	Explanation string // non-file text
}

// fileHeaderRe matches "--- FILE: path/to/file ---" (Format A).
var fileHeaderRe = regexp.MustCompile(`^---\s*FILE:\s*(.+?)\s*---\s*$`)

// fencedFileRe matches "```lang filename" or "```filename" (Format B).
var fencedFileRe = regexp.MustCompile("^```\\w*\\s+(.+\\..+)\\s*$")

// boldFileRe matches "**path/to/file**" or "**`path/to/file`**" (Format C — Opus style).
var boldFileRe = regexp.MustCompile("^\\*\\*`?([^*`]+\\.[a-zA-Z0-9]+)`?\\*\\*\\s*$")

// mdHeaderFileRe matches "### path/to/file" or "## path/to/file" (Format D — markdown header).
var mdHeaderFileRe = regexp.MustCompile(`^#{2,4}\s+` + "`?" + `([^\s` + "`" + `]+\.[a-zA-Z0-9]+)` + "`?" + `\s*$`)

// colonFileRe matches "File: path/to/file" or "FILE: path/to/file" (Format E — simple colon, no dashes).
var colonFileRe = regexp.MustCompile(`^[Ff][Ii]?[Ll][Ee]:\s*` + "`?" + `([^\s` + "`" + `]+\.[a-zA-Z0-9]+)` + "`?" + `\s*$`)

// fenceOpenRe matches any code fence opening.
var fenceOpenRe = regexp.MustCompile("^```")

// fenceCloseRe matches a code fence closing line.
var fenceCloseRe = regexp.MustCompile("^```\\s*$")

// genericFenceOpenRe matches fenced code blocks with optional info string.
// Examples:
//
//	```html
//	```python app.py
//	```js src/app.js
var genericFenceOpenRe = regexp.MustCompile("^```\\s*([^\\s`]+)?(?:\\s+(.+?))?\\s*$")

// plainPathLineRe matches path-only lines like:
//
//	index.html
//	`src/main.js`
//	- style.css
var plainPathLineRe = regexp.MustCompile(`^\s*(?:[-*]\s+)?` + "`?" + `([A-Za-z0-9_.\-\/]+\.[A-Za-z0-9]+)` + "`?" + `\s*:?\s*$`)

// ParseFiles extracts file blocks from AI response text.
// It supports multiple formats:
//
// Format A (Build phase):
//
//	--- FILE: path/to/file ---
//	```
//	content
//	```
//
// Format B (Chat, fenced with filename annotation):
//
//	```lang path/to/file
//	content
//	```
//
// Format C (Opus bold header):
//
//	**path/to/file**
//	```
//	content
//	```
//
// Format D (Markdown header):
//
//	### path/to/file
//	```
//	content
//	```
//
// Format E (Simple colon):
//
//	File: path/to/file
//	```
//	content
//	```
func ParseFiles(text string) ParseResult {
	lines := strings.Split(text, "\n")

	var files []ExtractedFile
	var explanation strings.Builder

	var currentPath string
	var currentContent strings.Builder
	inFile := false       // inside a file block's content
	waitingFence := false // saw FILE header, waiting for opening ```
	inFence := false      // inside a ``` ... ``` fence

	for _, line := range lines {
		switch {
		// State: not capturing a file — look for headers
		case !inFile && !waitingFence:
			// Check Format A header: --- FILE: path ---
			if m := fileHeaderRe.FindStringSubmatch(line); m != nil {
				waitingFence = true
				currentPath = strings.TrimSpace(m[1])
				currentContent.Reset()
				continue
			}

			// Check Format C: **path/to/file**
			if m := boldFileRe.FindStringSubmatch(line); m != nil {
				waitingFence = true
				currentPath = strings.TrimSpace(m[1])
				currentContent.Reset()
				continue
			}

			// Check Format D: ### path/to/file
			if m := mdHeaderFileRe.FindStringSubmatch(line); m != nil {
				waitingFence = true
				currentPath = strings.TrimSpace(m[1])
				currentContent.Reset()
				continue
			}

			// Check Format E: File: path/to/file
			if m := colonFileRe.FindStringSubmatch(line); m != nil {
				waitingFence = true
				currentPath = strings.TrimSpace(m[1])
				currentContent.Reset()
				continue
			}

			// Check Format B: ```lang filename.ext
			if m := fencedFileRe.FindStringSubmatch(line); m != nil {
				inFile = true
				inFence = true
				currentPath = strings.TrimSpace(m[1])
				currentContent.Reset()
				continue
			}

			explanation.WriteString(line + "\n")

		// State: saw "--- FILE: ... ---", waiting for opening fence
		case waitingFence:
			if fenceOpenRe.MatchString(line) {
				waitingFence = false
				inFile = true
				inFence = true
			} else if strings.TrimSpace(line) == "" {
				// skip blank lines between header and fence
			} else {
				// Not a fence — treat the header as false positive
				waitingFence = false
				explanation.WriteString("--- FILE: " + currentPath + " ---\n")
				explanation.WriteString(line + "\n")
				currentPath = ""
			}

		// State: capturing file content
		case inFile:
			if inFence && fenceCloseRe.MatchString(line) {
				// End of fenced block → save file
				content := currentContent.String()
				// Trim single trailing newline if present
				content = strings.TrimSuffix(content, "\n")
				files = append(files, ExtractedFile{
					Path:    currentPath,
					Content: content,
				})
				inFile = false
				inFence = false
				currentPath = ""
				currentContent.Reset()
			} else {
				currentContent.WriteString(line + "\n")
			}
		}
	}

	// If we were still capturing (unclosed fence), save what we have
	if inFile && currentPath != "" {
		content := strings.TrimSuffix(currentContent.String(), "\n")
		if content != "" {
			files = append(files, ExtractedFile{
				Path:    currentPath,
				Content: content,
			})
		}
	}

	return ParseResult{
		Files:       files,
		Explanation: strings.TrimRight(explanation.String(), "\n"),
	}
}

// ParseFilesBestEffort extends ParseFiles with fallback extraction for common
// LLM outputs that include fenced code but omit explicit --- FILE: headers.
// Fallback is intentionally conservative and should be used in build/review/fix
// flows, not generic chat extraction.
func ParseFilesBestEffort(text string) ParseResult {
	strict := ParseFiles(text)
	if len(strict.Files) > 0 {
		return strict
	}
	return parseBestEffortFencedFiles(text)
}

func parseBestEffortFencedFiles(text string) ParseResult {
	lines := strings.Split(text, "\n")
	totalFences := countFenceOpeners(lines)

	var files []ExtractedFile
	pathUseCount := make(map[string]int)

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "```") {
			continue
		}
		m := genericFenceOpenRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		lang := normalizeFenceLanguage(m[1])
		inlineHint := strings.TrimSpace(m[2])

		// Collect block content until closing fence.
		start := i + 1
		end := start
		for end < len(lines) && !fenceCloseRe.MatchString(strings.TrimSpace(lines[end])) {
			end++
		}
		if end >= len(lines) {
			// Unclosed fence: treat as non-file content.
			continue
		}
		content := strings.Join(lines[start:end], "\n")

		// Some models wrap a complete multi-file answer inside one outer code
		// fence. Reuse strict parsing on the inner content before heuristics.
		inner := ParseFiles(content)
		if len(inner.Files) > 0 {
			for _, f := range inner.Files {
				p, ok := normalizeLikelyPath(f.Path)
				if !ok {
					continue
				}
				p = uniquifyPath(p, pathUseCount)
				files = append(files, ExtractedFile{
					Path:    p,
					Content: f.Content,
				})
			}
			// Continue after closing fence.
			i = end
			continue
		}

		path := ""
		if p, ok := normalizeLikelyPath(inlineHint); ok {
			path = p
		}
		if path == "" {
			if p := findBestEffortPathHint(lines, i); p != "" {
				path = p
			}
		}
		if path == "" {
			path = inferPathFromFenceLanguage(lang, totalFences)
		}
		if path != "" {
			path = uniquifyPath(path, pathUseCount)
			files = append(files, ExtractedFile{
				Path:    path,
				Content: strings.TrimSuffix(content, "\n"),
			})
		}

		// Continue after closing fence.
		i = end
	}

	return ParseResult{Files: files, Explanation: strings.TrimSpace(text)}
}

func countFenceOpeners(lines []string) int {
	inFence := false
	count := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		if inFence {
			inFence = false
			continue
		}
		inFence = true
		count++
	}
	return count
}

func findBestEffortPathHint(lines []string, fenceLine int) string {
	for back := 1; back <= 3; back++ {
		idx := fenceLine - back
		if idx < 0 {
			break
		}
		line := strings.TrimSpace(lines[idx])
		if line == "" {
			continue
		}
		if m := plainPathLineRe.FindStringSubmatch(line); m != nil {
			if p, ok := normalizeLikelyPath(m[1]); ok {
				return p
			}
		}
		// If we hit non-empty narrative text, stop looking further up.
		break
	}
	return ""
}

func normalizeFenceLanguage(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	switch lang {
	case "javascript", "node", "nodejs":
		return "js"
	case "typescript":
		return "ts"
	case "py":
		return "python"
	case "yml":
		return "yaml"
	default:
		return lang
	}
}

func inferPathFromFenceLanguage(lang string, totalFences int) string {
	if lang == "" {
		return ""
	}
	// Avoid false positives: infer by language only when there are multiple
	// code fences (project-like output) or a single explicit html block.
	if totalFences < 2 && lang != "html" {
		return ""
	}
	switch lang {
	case "html":
		return "index.html"
	case "css":
		return "style.css"
	case "js":
		return "script.js"
	case "ts":
		return "script.ts"
	case "tsx":
		return "App.tsx"
	case "jsx":
		return "App.jsx"
	case "python":
		return "main.py"
	case "go":
		return "main.go"
	case "java":
		return "Main.java"
	case "json":
		return "data.json"
	case "yaml":
		return "config.yaml"
	case "sql":
		return "query.sql"
	case "bash", "sh", "shell":
		return "script.sh"
	case "md", "markdown":
		return "README.md"
	default:
		return ""
	}
}

func normalizeLikelyPath(raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", false
	}
	p = strings.Trim(p, "`*\"'")
	p = strings.TrimRight(p, ":")
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	// Disallow obvious non-path tokens.
	if strings.HasPrefix(strings.ToLower(p), "http://") || strings.HasPrefix(strings.ToLower(p), "https://") {
		return "", false
	}
	if strings.ContainsAny(p, " \t\r\n") {
		return "", false
	}
	if strings.Contains(p, "..") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return "", false
	}
	if !strings.Contains(p, ".") {
		return "", false
	}
	return p, true
}

func uniquifyPath(path string, seen map[string]int) string {
	seen[path]++
	if seen[path] == 1 {
		return path
	}
	suffix := seen[path]
	dot := strings.LastIndex(path, ".")
	if dot <= 0 || dot == len(path)-1 {
		return path + "-" + strconv.Itoa(suffix)
	}
	return fmt.Sprintf("%s-%d%s", path[:dot], suffix, path[dot:])
}

// Multiline versions for quick scanning across full text.
var fileHeaderReMulti = regexp.MustCompile(`(?m)^---\s*FILE:\s*.+?\s*---\s*$`)
var fencedFileReMulti = regexp.MustCompile("(?m)^```\\w*\\s+.+\\..+\\s*$")
var boldFileReMulti = regexp.MustCompile("(?m)^\\*\\*`?[^*`]+\\.[a-zA-Z0-9]+`?\\*\\*\\s*$")
var mdHeaderFileReMulti = regexp.MustCompile("(?m)^#{2,4}\\s+`?[^\\s`]+\\.[a-zA-Z0-9]+`?\\s*$")
var colonFileReMulti = regexp.MustCompile("(?mi)^[Ff][Ii]?[Ll][Ee]:\\s*`?[^\\s`]+\\.[a-zA-Z0-9]+`?\\s*$")

// ContainsFiles does a quick check whether text likely contains file blocks.
func ContainsFiles(text string) bool {
	return fileHeaderReMulti.MatchString(text) ||
		fencedFileReMulti.MatchString(text) ||
		boldFileReMulti.MatchString(text) ||
		mdHeaderFileReMulti.MatchString(text) ||
		colonFileReMulti.MatchString(text)
}
