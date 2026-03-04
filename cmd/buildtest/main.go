package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

// Headless test of the multi-model build pipeline:
//   Plan (gemini) → Code (claude) → Review (gemini) → Fix (codex)

var requestTimeout = flag.Duration("timeout", 60*time.Second, "per-step timeout for live provider calls")

func main() {
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load: %v\n", err)
		os.Exit(1)
	}

	modes := []model.UsageMode{
		model.ModeFree,
		model.ModeEconomy,
		model.ModeBalanced,
		model.ModePower,
	}

	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          makewand 多模型协作管道测试 (buildtest)                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("Request timeout: %s\n\n", requestTimeout.String())

	// Show available providers
	router := model.NewRouter(cfg)
	fmt.Println("▸ Available providers:")
	availableProviders := router.Available()
	for _, name := range availableProviders {
		fmt.Printf("  ✓ %s\n", name)
	}
	if len(availableProviders) == 0 {
		fmt.Println("  (none)")
	}
	fmt.Println()

	// Phase 1: Route decision table for build phases
	fmt.Println("═══ 1. Build Phase Route Table ═══")
	fmt.Println()

	phases := []struct {
		phase model.BuildPhase
		name  string
	}{
		{model.PhasePlan, "Plan"},
		{model.PhaseCode, "Code"},
		{model.PhaseReview, "Review"},
		{model.PhaseFix, "Fix"},
	}

	header := fmt.Sprintf("  %-16s", "Mode \\ Phase")
	for _, p := range phases {
		header += fmt.Sprintf("│ %-18s", p.name)
	}
	fmt.Println(header)
	fmt.Println("  " + strings.Repeat("─", 16) + strings.Repeat("┼"+strings.Repeat("─", 18), len(phases)))

	for _, mode := range modes {
		cfg.UsageMode = mode.String()
		r := model.NewRouter(cfg)

		row := fmt.Sprintf("  %-16s", mode.String())
		for _, p := range phases {
			primary := r.BuildProviderFor(p.phase)
			result, err := r.RouteProvider(primary, p.phase)
			if err != nil {
				row += fmt.Sprintf("│ %-18s", "✗ "+truncate(err.Error(), 14))
			} else {
				cell := result.Actual
				if result.IsFallback {
					cell += " ↩"
				}
				row += fmt.Sprintf("│ %-18s", cell)
			}
		}
		fmt.Println(row)
	}
	fmt.Println()

	// Phase 2: Cross-model verification (ensure Review ≠ Code, Fix ≠ Code)
	fmt.Println("═══ 2. Cross-model Verification ═══")
	fmt.Println()
	allOK := true
	for _, mode := range modes {
		cfg.UsageMode = mode.String()
		r := model.NewRouter(cfg)

		codeProv := r.BuildProviderFor(model.PhaseCode)
		reviewProv := r.BuildProviderFor(model.PhaseReview)
		fixProv := r.BuildProviderFor(model.PhaseFix)

		crossReview := codeProv != reviewProv
		crossFix := codeProv != fixProv

		status := "✓"
		if !crossReview || !crossFix {
			status = "✗"
			allOK = false
		}

		fmt.Printf("  %s %-10s  Code=%s  Review=%s(cross=%v)  Fix=%s(cross=%v)\n",
			status, mode.String(), codeProv, reviewProv, crossReview, fixProv, crossFix)
	}
	if allOK {
		fmt.Println("  → All modes have cross-model review and fix")
	} else {
		fmt.Println("  → WARNING: Some modes share providers (may be expected for Free mode with limited providers)")
	}
	fmt.Println()

	// Phase 3: Live pipeline test (balanced mode)
	fmt.Println("═══ 3. Live Pipeline Test (balanced mode) ═══")
	fmt.Println()

	cfg.UsageMode = "balanced"
	router = model.NewRouter(cfg)

	// Step 1: Plan
	fmt.Print("  [1/4] Plan... ")
	planProvider := router.BuildProviderFor(model.PhasePlan)
	start := time.Now()
	planContent, planUsage, planResult, err := callChatWith(router, *requestTimeout, planProvider, model.PhasePlan,
		[]model.Message{{Role: "user", Content: "I want to create a simple calculator web app with HTML/CSS/JS. Provide a brief plan with file structure. Keep it very short."}},
		"You are a friendly project planner. Be very concise.")
	elapsed := time.Since(start)
	if err != nil {
		fmt.Printf("✗ %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ %s (%.1fs, %d tokens)\n", planResult.Actual, elapsed.Seconds(), planUsage.InputTokens+planUsage.OutputTokens)
	printTruncated(planContent, 3)

	// Step 2: Code Gen
	fmt.Print("  [2/4] Code... ")
	codeProvider := router.BuildProviderFor(model.PhaseCode)
	start = time.Now()
	codeContent, codeUsage, codeResult, err := callChatWith(router, *requestTimeout, codeProvider, model.PhaseCode,
		[]model.Message{{Role: "user", Content: "Create a simple calculator web app. Output 2-3 files max. Use this format:\n\n--- FILE: path/to/file ---\n```\ncontent\n```\n\nKeep it minimal."}},
		"You are an expert programmer. Generate working code. Be concise.")
	elapsed = time.Since(start)
	if err != nil {
		fmt.Printf("✗ %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ %s (%.1fs, %d tokens)\n", codeResult.Actual, elapsed.Seconds(), codeUsage.InputTokens+codeUsage.OutputTokens)

	// Parse files from code output
	parsed := engine.ParseFilesBestEffort(codeContent)
	fmt.Printf("         Parsed %d files: ", len(parsed.Files))
	for i, f := range parsed.Files {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(f.Path)
	}
	fmt.Println()

	// Step 3: Review
	fmt.Print("  [3/4] Review... ")
	reviewProvider := router.BuildProviderFor(model.PhaseReview)
	if reviewProvider == codeResult.Actual {
		// Force different provider
		for _, fb := range []string{"gemini", "codex", "ollama", "claude"} {
			if fb != codeResult.Actual {
				reviewProvider = fb
				break
			}
		}
	}

	reviewPrompt := fmt.Sprintf(
		"Review the following code for bugs and issues. If code is fine, respond with: LGTM\n\nIf issues exist, output corrected files using:\n--- FILE: path ---\n```\ncontent\n```\n\n%s",
		codeContent)

	start = time.Now()
	reviewContent, reviewUsage, reviewResult, reviewErr := callChatWith(router, *requestTimeout, reviewProvider, model.PhaseReview,
		[]model.Message{{Role: "user", Content: reviewPrompt}},
		"You are an expert code reviewer. Be concise.")
	elapsed = time.Since(start)
	if reviewErr != nil {
		fmt.Printf("✗ %v\n", reviewErr)
		fmt.Println("         (review error is non-fatal, continuing)")
	} else {
		hasIssues := !isLGTMResponse(reviewContent)
		status := "LGTM"
		if hasIssues {
			reviewParsed := engine.ParseFilesBestEffort(reviewContent)
			status = fmt.Sprintf("%d fixes", len(reviewParsed.Files))
		}
		fmt.Printf("✓ %s → %s (%.1fs, %d tokens)\n", reviewResult.Actual, status, elapsed.Seconds(), reviewUsage.InputTokens+reviewUsage.OutputTokens)
	}

	// Step 4: Fix (simulate with a fake error)
	fmt.Print("  [4/4] Fix (dry run)... ")
	fixProvider := router.BuildProviderFor(model.PhaseFix)
	if fixProvider == codeResult.Actual {
		for _, fb := range []string{"codex", "gemini", "ollama", "claude"} {
			if fb != codeResult.Actual {
				fixProvider = fb
				break
			}
		}
	}

	start = time.Now()
	_, fixUsage, fixResult, fixErr := callChatWith(router, *requestTimeout, fixProvider, model.PhaseFix,
		[]model.Message{{Role: "user", Content: "The following test error occurred:\n\n```\nReferenceError: calculate is not defined\n```\n\nFix the code. Output corrected file using:\n--- FILE: path ---\n```\ncontent\n```"}},
		"You are an expert programmer fixing errors. Be concise.")
	elapsed = time.Since(start)
	if fixErr != nil {
		fmt.Printf("✗ %v\n", fixErr)
	} else {
		fmt.Printf("✓ %s (%.1fs, %d tokens)\n", fixResult.Actual, elapsed.Seconds(), fixUsage.InputTokens+fixUsage.OutputTokens)
	}

	// Summary
	fmt.Println()
	fmt.Println("═══ Pipeline Summary ═══")
	fmt.Println()
	fmt.Printf("  Plan:   %s\n", planResult.Actual)
	fmt.Printf("  Code:   %s\n", codeResult.Actual)
	if reviewErr == nil {
		fmt.Printf("  Review: %s (cross-model: %v)\n", reviewResult.Actual, reviewResult.Actual != codeResult.Actual)
	}
	if fixErr == nil {
		fmt.Printf("  Fix:    %s (cross-model: %v)\n", fixResult.Actual, fixResult.Actual != codeResult.Actual)
	}

	totalCost := planUsage.Cost + codeUsage.Cost + reviewUsage.Cost + fixUsage.Cost
	if totalCost > 0 {
		fmt.Printf("  Total cost: $%.4f\n", totalCost)
	} else {
		fmt.Println("  Total cost: free (subscription/local)")
	}
	fmt.Println()
	fmt.Println("═══ Done ═══")
}

func callChatWith(
	router *model.Router,
	timeout time.Duration,
	name string,
	phase model.BuildPhase,
	messages []model.Message,
	system string,
	exclude ...string,
) (string, model.Usage, model.RouteResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return router.ChatWith(ctx, name, phase, messages, system, exclude...)
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max]
}

func printTruncated(content string, maxLines int) {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for i, line := range lines {
		if i >= maxLines {
			fmt.Printf("         ... (+%d more lines)\n", len(lines)-maxLines)
			break
		}
		fmt.Printf("         %s\n", line)
	}
}

func isLGTMResponse(content string) bool {
	return strings.EqualFold(strings.TrimSpace(content), "LGTM")
}
