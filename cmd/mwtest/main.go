package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/diag"
	"github.com/makewand/makewand/internal/model"
)

// test case definition
type testCase struct {
	name   string
	task   model.TaskType
	prompt string
}

var tests = []testCase{
	{
		name:   "Analyze: plan a todo app",
		task:   model.TaskAnalyze,
		prompt: "I want to build a simple todo list web app. What tech stack should I use and what are the main features? Keep it under 200 words.",
	},
	{
		name:   "Code: generate a function",
		task:   model.TaskCode,
		prompt: "Write a Python function that takes a list of integers and returns the top 3 most frequent elements. Include type hints and a docstring. Only output the code, no explanation.",
	},
	{
		name:   "Explain: what is REST API",
		task:   model.TaskExplain,
		prompt: "Explain what a REST API is to someone who has never programmed before. Use a real-world analogy. Keep it under 150 words.",
	},
	{
		name:   "Review: find bugs",
		task:   model.TaskReview,
		prompt: "Review this code for bugs:\n\n```python\ndef divide_list(numbers, divisor):\n    results = []\n    for n in numbers:\n        results.append(n / divisor)\n    return results\n\nprint(divide_list([10, 20, 30], 0))\n```\n\nList the issues found. Be concise.",
	},
}

var requestTimeout = flag.Duration("timeout", 45*time.Second, "per-request timeout for live provider calls")

func main() {
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		diag.Stderr().ErrorErr("config load failed", err)
		os.Exit(1)
	}

	// Determine which modes/providers to test
	modes := []struct {
		name string
		mode model.UsageMode
	}{
		{"free", model.ModeFree},
		{"balanced", model.ModeBalanced},
		{"power", model.ModePower},
	}

	// Also test specific providers directly
	directProviders := []string{"gemini", "claude", "codex"}

	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("makewand Provider & Mode Evaluation")
	fmt.Printf("CLIs detected: %d\n", len(cfg.CLIs))
	fmt.Printf("Request timeout: %s\n", requestTimeout.String())
	for _, cli := range cfg.CLIs {
		fmt.Printf("  %s (%s)\n", cli.Name, cli.Version)
	}
	fmt.Println(strings.Repeat("=", 80))

	// Part 1: Test modes (router chooses provider)
	fmt.Println("\n## PART 1: Mode-based routing (router selects provider)")
	fmt.Println(strings.Repeat("-", 80))

	for _, m := range modes {
		fmt.Printf("\n### Mode: %s\n", strings.ToUpper(m.name))

		router := model.NewRouter(cfg)
		router.SetMode(m.mode)

		for _, tc := range tests {
			runTest(router, tc, *requestTimeout)
		}
	}

	// Part 2: Direct provider tests (force specific provider)
	fmt.Println("\n\n## PART 2: Direct provider comparison (same prompt)")
	fmt.Println(strings.Repeat("-", 80))

	// Use just the code generation test for direct comparison
	codeTest := tests[1] // "Code: generate a function"

	for _, provName := range directProviders {
		fmt.Printf("\n### Provider: %s\n", provName)

		router := model.NewRouter(cfg)
		p, err := router.Get(provName)
		if err != nil {
			fmt.Printf("  SKIP: %v\n", err)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), *requestTimeout)
		start := time.Now()
		content, usage, err := p.Chat(ctx, []model.Message{
			{Role: "user", Content: codeTest.prompt},
		}, "You are a helpful programming assistant.", 4096)
		elapsed := time.Since(start)
		cancel()

		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			continue
		}

		printResult(provName, elapsed, usage, content)
	}

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("Evaluation complete.")
}

func runTest(router *model.Router, tc testCase, timeout time.Duration) {
	fmt.Printf("\n  Task: %s\n", tc.name)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	content, usage, result, err := router.Chat(ctx, tc.task, []model.Message{
		{Role: "user", Content: tc.prompt},
	}, "You are a helpful assistant for non-programmers.")
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("    ERROR: %v\n", err)
		return
	}

	provider := result.Actual
	if result.IsFallback {
		provider += fmt.Sprintf(" (fallback from %s)", result.Requested)
	}

	printResult(provider, elapsed, usage, content)
}

func printResult(provider string, elapsed time.Duration, usage model.Usage, content string) {
	fmt.Printf("    Provider: %s | Model: %s\n", provider, usage.Model)
	fmt.Printf("    Time: %.1fs | Tokens: %d in / %d out | Cost: $%.4f\n",
		elapsed.Seconds(), usage.InputTokens, usage.OutputTokens, usage.Cost)

	// Truncate content for display
	lines := strings.Split(strings.TrimSpace(content), "\n")
	preview := strings.Join(lines, "\n")
	if len(preview) > 500 {
		preview = preview[:500] + "\n    ... (truncated)"
	}

	fmt.Println("    ---")
	for _, line := range strings.Split(preview, "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println("    ---")
}
