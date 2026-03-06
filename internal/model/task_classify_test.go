package model

import "testing"

func TestClassifyTask(t *testing.T) {
	tests := []struct {
		prompt string
		want   TaskType
	}{
		{prompt: "/review this", want: TaskReview},
		{prompt: "please review.", want: TaskReview},
		{prompt: "please fix this bug", want: TaskFix},
		{prompt: "fix, please", want: TaskFix},
		{prompt: "bug?", want: TaskFix},
		{prompt: "explain rest api", want: TaskExplain},
		{prompt: "how does this work", want: TaskExplain},
		{prompt: "what is this?", want: TaskExplain},
		{prompt: "plan this feature", want: TaskAnalyze},
		{prompt: "design the api", want: TaskAnalyze},
		{prompt: "write code", want: TaskCode},
		{prompt: "checkout the repo", want: TaskCode},
		{prompt: "handle errors gracefully", want: TaskCode},
	}

	for _, tt := range tests {
		if got := ClassifyTask(tt.prompt); got != tt.want {
			t.Fatalf("ClassifyTask(%q) = %v, want %v", tt.prompt, got, tt.want)
		}
	}
}
