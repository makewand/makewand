package main

import (
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/model"
)

func TestHeadlessCodeOnlyRequested_ByTask(t *testing.T) {
	if !headlessCodeOnlyRequested(model.TaskCode, "hello") {
		t.Fatal("headlessCodeOnlyRequested(TaskCode) = false, want true")
	}
}

func TestSanitizeHeadlessContent_ExtractsFileBlock(t *testing.T) {
	prompt := "Return only the complete content of solution.js. Do not output markdown."
	raw := "summary line\n--- FILE: solution.js ---\n```js\nfunction x() {\n  return 1;\n}\n```\n"

	got := sanitizeHeadlessContent(prompt, model.TaskCode, raw)
	if !strings.Contains(got, "function x()") {
		t.Fatalf("sanitizeHeadlessContent() = %q, want extracted file block", got)
	}
	if strings.Contains(got, "summary line") {
		t.Fatalf("sanitizeHeadlessContent() = %q, should remove preamble", got)
	}
}

func TestSanitizeHeadlessContent_ExtractsCodeFence(t *testing.T) {
	prompt := "Output only complete content. No markdown."
	raw := "Here you go:\n```go\npackage demo\n\nfunc A() {}\n```\n"

	got := sanitizeHeadlessContent(prompt, model.TaskCode, raw)
	if !strings.HasPrefix(strings.TrimSpace(got), "package demo") {
		t.Fatalf("sanitizeHeadlessContent() = %q, want fenced code payload", got)
	}
}

func TestSanitizeHeadlessContent_StripsLeadingNarration(t *testing.T) {
	prompt := "Return only the complete content of retry.go. No explanations."
	raw := "The file has been written.\npackage retrycase\n\nfunc RetryHTTP() {}\n"

	got := sanitizeHeadlessContent(prompt, model.TaskCode, raw)
	if strings.Contains(got, "The file has been written") {
		t.Fatalf("sanitizeHeadlessContent() = %q, should strip narrative preface", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "package retrycase") {
		t.Fatalf("sanitizeHeadlessContent() = %q, want code body", got)
	}
}

func TestSanitizeHeadlessContent_NonCodePromptUnchanged(t *testing.T) {
	raw := "This is a plain explanation."
	got := sanitizeHeadlessContent("explain this", model.TaskExplain, raw)
	if got != raw {
		t.Fatalf("sanitizeHeadlessContent() = %q, want unchanged %q", got, raw)
	}
}

