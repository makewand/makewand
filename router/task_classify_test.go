package router

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
		{prompt: "你是谁？", want: TaskExplain},
		{prompt: "请解释这个函数", want: TaskExplain},
		{prompt: "plan this feature", want: TaskAnalyze},
		{prompt: "design the api", want: TaskAnalyze},
		{prompt: "请设计一个方案", want: TaskAnalyze},
		{prompt: "write code", want: TaskCode},
		{prompt: "checkout the repo", want: TaskCode},
		{prompt: "实现一个登录接口", want: TaskCode},
		{prompt: "handle errors gracefully", want: TaskCode},
		{prompt: "修复这个报错", want: TaskFix},
		{prompt: "请做代码审查", want: TaskReview},
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
