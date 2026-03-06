package model

import "testing"

func FuzzClassifyTask(f *testing.F) {
	seeds := []string{
		"",
		"please review.",
		"fix, please",
		"bug?",
		"checkout the repo",
		"how does this work",
		"/review this",
		"/fix this",
		"设计一个 API",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		got := ClassifyTask(input)
		if got < TaskAnalyze || got > TaskFix {
			t.Fatalf("ClassifyTask(%q) = %v, want known TaskType enum", input, got)
		}

		words := classifyWords(input)
		for _, word := range words {
			if word == "" {
				t.Fatalf("classifyWords(%q) returned empty token", input)
			}
		}
	})
}
