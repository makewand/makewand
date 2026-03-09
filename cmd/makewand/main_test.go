package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/model"
)

func TestShouldUseHeadless(t *testing.T) {
	tests := []struct {
		name           string
		prompt         string
		printFlag      bool
		interactiveTTY bool
		want           bool
	}{
		{
			name:           "no prompt never headless",
			prompt:         "",
			printFlag:      true,
			interactiveTTY: false,
			want:           false,
		},
		{
			name:           "print flag forces headless",
			prompt:         "hello",
			printFlag:      true,
			interactiveTTY: true,
			want:           true,
		},
		{
			name:           "non-tty prompt defaults to headless",
			prompt:         "hello",
			printFlag:      false,
			interactiveTTY: false,
			want:           true,
		},
		{
			name:           "tty prompt stays interactive",
			prompt:         "hello",
			printFlag:      false,
			interactiveTTY: true,
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldUseHeadless(tt.prompt, tt.printFlag, tt.interactiveTTY)
			if got != tt.want {
				t.Fatalf("shouldUseHeadless(%q, %v, %v) = %v, want %v", tt.prompt, tt.printFlag, tt.interactiveTTY, got, tt.want)
			}
		})
	}
}

func TestClassifyPromptTask(t *testing.T) {
	tests := []struct {
		prompt string
		want   model.TaskType
	}{
		{prompt: "/review this", want: model.TaskReview},
		{prompt: "please review.", want: model.TaskReview},
		{prompt: "please fix this bug", want: model.TaskFix},
		{prompt: "fix, please", want: model.TaskFix},
		{prompt: "bug?", want: model.TaskFix},
		{prompt: "explain rest api", want: model.TaskExplain},
		{prompt: "plan this feature", want: model.TaskAnalyze},
		{prompt: "write code", want: model.TaskCode},
		{prompt: "checkout the repo", want: model.TaskCode},
		{prompt: "design the api", want: model.TaskAnalyze},
		{prompt: "how does this work", want: model.TaskExplain},
		{prompt: "there is an error here", want: model.TaskFix},
		{prompt: "handle errors gracefully", want: model.TaskCode},
	}

	for _, tt := range tests {
		if got := classifyPromptTask(tt.prompt); got != tt.want {
			t.Fatalf("classifyPromptTask(%q) = %v, want %v", tt.prompt, got, tt.want)
		}
	}
}

func TestPromptTaskToBuildPhase(t *testing.T) {
	tests := []struct {
		task model.TaskType
		want model.BuildPhase
	}{
		{task: model.TaskAnalyze, want: model.PhasePlan},
		{task: model.TaskCode, want: model.PhaseCode},
		{task: model.TaskReview, want: model.PhaseReview},
		{task: model.TaskFix, want: model.PhaseFix},
	}

	for _, tt := range tests {
		if got := promptTaskToBuildPhase(tt.task); got != tt.want {
			t.Fatalf("promptTaskToBuildPhase(%v) = %v, want %v", tt.task, got, tt.want)
		}
	}
}

func TestNewHeadlessTraceSinkUsesConfigDir(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)

	sink, path, err := newHeadlessTraceSink()
	if err != nil {
		t.Fatalf("newHeadlessTraceSink() error: %v", err)
	}
	defer sink.Close()

	want := filepath.Join(cfgDir, "trace.jsonl")
	if path != want {
		t.Fatalf("newHeadlessTraceSink() path = %q, want %q", path, want)
	}
}

func TestHeadlessTraceSinkWritesEvent(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MAKEWAND_CONFIG_DIR", cfgDir)

	sink, path, err := newHeadlessTraceSink()
	if err != nil {
		t.Fatalf("newHeadlessTraceSink() error: %v", err)
	}

	event := model.TraceEvent{
		Timestamp: time.Now().UTC(),
		Event:     "test_event",
		Selected:  "gemini",
	}
	sink.Trace(event)
	if err := sink.Close(); err != nil {
		t.Fatalf("sink.Close() error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error: %v", path, err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("trace file is empty")
	}
	if !strings.Contains(line, "\"event\":\"test_event\"") {
		t.Fatalf("trace line missing event field: %s", line)
	}
}
