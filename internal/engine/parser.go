package engine

import (
	"regexp"
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
