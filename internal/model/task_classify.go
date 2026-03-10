package model

import (
	"strings"
	"unicode"
)

// ClassifyTask determines the task intent from user input using slash commands,
// normalized token matching, and a small set of phrase rules.
func ClassifyTask(input string) TaskType {
	lower := strings.ToLower(strings.TrimSpace(input))

	switch {
	case strings.HasPrefix(lower, "/review"):
		return TaskReview
	case strings.HasPrefix(lower, "/fix"):
		return TaskFix
	case strings.HasPrefix(lower, "/ask"), strings.HasPrefix(lower, "/explain"):
		return TaskExplain
	case strings.HasPrefix(lower, "/plan"):
		return TaskAnalyze
	}

	// Basic CJK intent hints for common Chinese prompts.
	// Keep this lightweight and deterministic (no heavy NLP dependency).
	switch {
	case containsAny(lower, "修复", "报错", "错误", "异常", "故障"):
		return TaskFix
	case containsAny(lower, "审查", "评审", "review"):
		return TaskReview
	case containsAny(lower, "规划", "方案", "设计", "架构"):
		return TaskAnalyze
	case containsAny(lower, "实现", "编写", "生成代码", "写代码"):
		return TaskCode
	case containsAny(lower, "解释", "说明", "为什么", "为何", "怎么", "如何", "谁", "什么"):
		return TaskExplain
	}

	words := classifyWords(lower)
	wordSet := make(map[string]bool, len(words))
	for _, word := range words {
		wordSet[word] = true
	}
	codeIntent := hasAnyClassifyWord(
		wordSet,
		"implement", "create", "build", "write", "generate", "code", "function", "module", "class", "refactor",
		"add", "remove", "delete", "update", "change", "modify", "move", "rename", "handle", "checkout", "setup", "install", "configure",
	) ||
		containsClassifyPhrase(words, "complete function") ||
		containsClassifyPhrase(words, "complete content") ||
		containsClassifyPhrase(words, "return only the complete content") ||
		containsClassifyPhrase(words, "output only the complete content")

	if hasAnyClassifyWord(wordSet, "review", "check", "audit") {
		return TaskReview
	}
	if hasAnyClassifyWord(wordSet, "fix", "bug", "error", "broken") {
		// Prompts that ask to implement/build code often mention "error" as a requirement.
		// Treat those as code generation unless they explicitly ask to fix broken code.
		if codeIntent && !hasAnyClassifyWord(wordSet, "fix", "bug", "broken") {
			return TaskCode
		}
		return TaskFix
	}
	if hasAnyClassifyWord(wordSet, "explain", "why") ||
		containsClassifyPhrase(words, "how does") ||
		containsClassifyPhrase(words, "what does") ||
		containsClassifyPhrase(words, "what is") {
		return TaskExplain
	}
	if hasAnyClassifyWord(wordSet, "plan", "analyze", "design", "architect") {
		return TaskAnalyze
	}
	if codeIntent {
		return TaskCode
	}

	return TaskExplain
}

func containsAny(input string, keywords ...string) bool {
	for _, kw := range keywords {
		if kw != "" && strings.Contains(input, kw) {
			return true
		}
	}
	return false
}

func hasAnyClassifyWord(wordSet map[string]bool, keywords ...string) bool {
	for _, keyword := range keywords {
		if wordSet[keyword] {
			return true
		}
	}
	return false
}

func containsClassifyPhrase(words []string, phrase string) bool {
	phraseWords := classifyWords(phrase)
	if len(phraseWords) == 0 || len(phraseWords) > len(words) {
		return false
	}

	for i := 0; i <= len(words)-len(phraseWords); i++ {
		matched := true
		for j := range phraseWords {
			if words[i+j] != phraseWords[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func classifyWords(input string) []string {
	var (
		words []string
		buf   strings.Builder
	)

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		words = append(words, buf.String())
		buf.Reset()
	}

	for _, r := range input {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return words
}
