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
		{
			prompt: "You are fixing production HTTP reliability code in Go. Task: Implement RetryHTTP and return only the complete content of retry.go. retry on client.Do error.",
			want:   TaskCode,
		},
		{
			prompt: "implement function verifyWebhookSignature and return only the complete content of solution.js. no explanations. reject malformed signature errors.",
			want:   TaskCode,
		},
	}

	for _, tt := range tests {
		if got := ClassifyTask(tt.prompt); got != tt.want {
			t.Fatalf("ClassifyTask(%q) = %v, want %v", tt.prompt, got, tt.want)
		}
	}
}
