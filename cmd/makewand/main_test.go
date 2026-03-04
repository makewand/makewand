package main

import (
	"testing"

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
		{prompt: "please fix this bug", want: model.TaskFix},
		{prompt: "explain rest api", want: model.TaskExplain},
		{prompt: "plan this feature", want: model.TaskAnalyze},
		{prompt: "write code", want: model.TaskCode},
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
