package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/diag"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

// Quality comparison: same prompt across 4 modes + cross-model review of each output.

const codePrompt = `Create a password strength checker web app with:
1. An input field for the password
2. Real-time strength indicator (weak/medium/strong/very strong)
3. Show which criteria are met: length>=8, uppercase, lowercase, digit, special char
4. Color-coded visual feedback

Output files using:
--- FILE: path/to/file ---
` + "```" + `
content
` + "```" + `

Generate ALL files needed.`

const codeSystem = "You are an expert programmer. Generate complete, working, production-quality code. Include proper error handling and edge cases."

const reviewPrompt = `Review this code for:
1. Bugs and logic errors
2. Security issues (XSS, injection, etc.)
3. Edge cases not handled
4. Code quality (readability, best practices)

Rate overall quality 1-10 and explain briefly.
Respond in this format:

SCORE: X/10
ISSUES: (list issues, or "none")
VERDICT: (one sentence summary)

Code to review:

%s`

type modeResult struct {
	mode           string
	provider       string
	modelID        string
	tier           string
	elapsed        time.Duration
	tokens         int
	cost           float64
	files          []engine.ExtractedFile
	rawOutput      string
	rawPath        string
	reviewScore    string
	reviewVerdict  string
	reviewProvider string
	reviewElapsed  time.Duration
	err            error
	reviewErr      error
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		diag.Stderr().ErrorErr("config load failed", err)
		os.Exit(1)
	}

	modes := []struct {
		mode model.UsageMode
		name string
		tier string
	}{
		{model.ModeFree, "Free", "Cheap (free models)"},
		{model.ModeEconomy, "Economy", "Cheap (prefer free)"},
		{model.ModeBalanced, "Balanced", "Mid (quality/cost)"},
		{model.ModePower, "Power", "Premium (best)"},
	}

	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║        makewand 代码质量对比测试 (qualtest)                     ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  同一 prompt → 4 种模式生成 → 交叉审查 → 质量评分              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  Prompt: %s\n", strings.Split(codePrompt, "\n")[0])
	fmt.Println()

	// Show providers
	tmpRouter := model.NewRouter(cfg)
	fmt.Print("  Providers: ")
	fmt.Println(strings.Join(tmpRouter.Available(), ", "))
	fmt.Println()

	var results []modeResult

	// Generate code in each mode
	for i, m := range modes {
		fmt.Printf("━━━ [%d/4] %s mode (%s) ━━━\n", i+1, m.name, m.tier)

		cfg.UsageMode = m.mode.String()
		router := model.NewRouter(cfg)
		ctx := context.Background()

		codeProvider := router.BuildProviderForAdaptive(model.PhaseCode)
		fmt.Printf("  Code provider: %s\n", codeProvider)

		r := modeResult{mode: m.name, tier: m.tier}

		// Generate
		fmt.Print("  Generating... ")
		start := time.Now()
		content, usage, result, err := callChatBestWithTimeout(ctx, router, 3*time.Minute, model.PhaseCode,
			[]model.Message{{Role: "user", Content: codePrompt}}, codeSystem)
		r.elapsed = time.Since(start)

		if err != nil {
			r.err = err
			fmt.Printf("✗ %v\n", err)
			results = append(results, r)
			fmt.Println()
			continue
		}

		totalUsage := usage
		finalResult := result
		parsed := engine.ParseFilesBestEffort(content)
		if len(parsed.Files) == 0 {
			fmt.Print("  No files parsed, retrying with strict format... ")
			retryStart := time.Now()
			retryContent, retryUsage, retryResult, retryErr := retryCodeOutputForFiles(ctx, router, codePrompt, content)
			retryElapsed := time.Since(retryStart)
			r.elapsed += retryElapsed
			if retryErr != nil {
				fmt.Printf("✗ %v\n", retryErr)
			} else {
				retryParsed := engine.ParseFilesBestEffort(retryContent)
				if len(retryParsed.Files) > 0 {
					content = retryContent
					parsed = retryParsed
					finalResult = retryResult
					totalUsage.InputTokens += retryUsage.InputTokens
					totalUsage.OutputTokens += retryUsage.OutputTokens
					totalUsage.Cost += retryUsage.Cost
					fmt.Printf("✓ recovered %d files (%.1fs)\n", len(retryParsed.Files), retryElapsed.Seconds())
				} else {
					fmt.Printf("✗ still no files (%.1fs)\n", retryElapsed.Seconds())
				}
			}
		}

		r.provider = finalResult.Actual
		r.modelID = finalResult.ModelID
		r.tokens = totalUsage.InputTokens + totalUsage.OutputTokens
		r.cost = totalUsage.Cost
		r.rawOutput = content
		if tmp, createErr := os.CreateTemp("", fmt.Sprintf("qualtest-%s-*.txt", strings.ToLower(m.name))); createErr == nil {
			if _, writeErr := tmp.WriteString(content); writeErr == nil {
				r.rawPath = tmp.Name()
			}
			_ = tmp.Close()
		}
		r.files = parsed.Files

		fmt.Printf("✓ %.1fs, %d tokens", r.elapsed.Seconds(), r.tokens)
		if r.cost > 0 {
			fmt.Printf(", $%.4f", r.cost)
		}
		fmt.Println()

		fmt.Printf("  Files (%d): ", len(r.files))
		for j, f := range r.files {
			lines := strings.Count(f.Content, "\n") + 1
			if j > 0 {
				fmt.Print(", ")
			}
			fmt.Printf("%s (%d lines)", f.Path, lines)
		}
		fmt.Println()
		if len(r.files) == 0 && r.rawPath != "" {
			fmt.Printf("  Raw output saved: %s\n", r.rawPath)
		}

		// Cross-model review — use a DIFFERENT provider than the one that wrote code
		reviewProvider := router.BuildProviderForAdaptive(model.PhaseReview)
		fmt.Printf("  Reviewing (%s → %s)... ", finalResult.Actual, reviewProvider)
		rPrompt := fmt.Sprintf(reviewPrompt, content)
		start = time.Now()
		reviewContent, _, reviewResult, rerr := callChatBestWithTimeout(ctx, router, 3*time.Minute, model.PhaseReview,
			[]model.Message{{Role: "user", Content: rPrompt}},
			"You are a senior code reviewer. Be honest and concise. Rate strictly.",
			finalResult.Actual)
		r.reviewElapsed = time.Since(start)

		if rerr != nil {
			r.reviewErr = rerr
			fmt.Printf("✗ %v\n", rerr)
		} else {
			r.reviewProvider = reviewResult.Actual
			// Extract score and verdict
			for _, line := range strings.Split(reviewContent, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "SCORE:") {
					r.reviewScore = strings.TrimSpace(strings.TrimPrefix(line, "SCORE:"))
				}
				if strings.HasPrefix(line, "VERDICT:") {
					r.reviewVerdict = strings.TrimSpace(strings.TrimPrefix(line, "VERDICT:"))
				}
			}
			if r.reviewScore == "" {
				// Try to find any X/10 pattern
				for _, line := range strings.Split(reviewContent, "\n") {
					if strings.Contains(line, "/10") {
						r.reviewScore = strings.TrimSpace(line)
						if len(r.reviewScore) > 30 {
							r.reviewScore = r.reviewScore[:30]
						}
						break
					}
				}
			}
			if r.reviewScore == "" {
				r.reviewScore = "N/A"
			}
			fmt.Printf("✓ Score: %s (%.1fs)\n", r.reviewScore, r.reviewElapsed.Seconds())
			if r.reviewVerdict != "" {
				fmt.Printf("  Verdict: %s\n", r.reviewVerdict)
			}
		}

		results = append(results, r)
		fmt.Println()
	}

	// Summary table
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                             对比结果总结                                       ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Printf("  %-10s │ %-12s │ %5s │ %6s │ %7s │ %8s │ %s\n",
		"Mode", "Provider", "Files", "Time", "Tokens", "Cost", "Review Score")
	fmt.Println("  " + strings.Repeat("─", 10) + "─┼─" + strings.Repeat("─", 12) + "─┼─" + strings.Repeat("─", 5) +
		"─┼─" + strings.Repeat("─", 6) + "─┼─" + strings.Repeat("─", 7) + "─┼─" + strings.Repeat("─", 8) + "─┼─" + strings.Repeat("─", 20))

	for _, r := range results {
		if r.err != nil {
			fmt.Printf("  %-10s │ %-12s │ %5s │ %6s │ %7s │ %8s │ %s\n",
				r.mode, "ERROR", "-", "-", "-", "-", r.err.Error())
			continue
		}

		costStr := "free"
		if r.cost > 0 {
			costStr = fmt.Sprintf("$%.4f", r.cost)
		}

		fmt.Printf("  %-10s │ %-12s │ %5d │ %5.1fs │ %7d │ %8s │ %s\n",
			r.mode, r.provider, len(r.files), r.elapsed.Seconds(), r.tokens, costStr, r.reviewScore)
	}
	fmt.Println()

	// Show code samples side by side (first file from each)
	fmt.Println("═══ 各模式代码片段对比 (主文件前 30 行) ═══")
	for _, r := range results {
		if r.err != nil || len(r.files) == 0 {
			continue
		}

		// Find the HTML file (main entry point)
		var mainFile engine.ExtractedFile
		for _, f := range r.files {
			if strings.HasSuffix(f.Path, ".html") {
				mainFile = f
				break
			}
		}
		if mainFile.Path == "" {
			mainFile = r.files[0]
		}

		fmt.Printf("\n── %s (%s) → %s ──\n", r.mode, r.provider, mainFile.Path)
		lines := strings.Split(mainFile.Content, "\n")
		for i, line := range lines {
			if i >= 30 {
				fmt.Printf("  ... (+%d more lines)\n", len(lines)-30)
				break
			}
			fmt.Printf("  %s\n", line)
		}
	}

	// Verdicts
	fmt.Println()
	fmt.Println("═══ 审查评语 ═══")
	for _, r := range results {
		if r.reviewVerdict != "" {
			fmt.Printf("  %-10s (%s→%s): %s\n", r.mode, r.provider, r.reviewProvider, r.reviewVerdict)
		}
	}
	fmt.Println()
	fmt.Println("═══ 测试完成 ═══")
}

func retryCodeOutputForFiles(
	ctx context.Context,
	router *model.Router,
	originalPrompt string,
	previousOutput string,
) (string, model.Usage, model.RouteResult, error) {
	retryPrompt := fmt.Sprintf(
		"The previous response did not provide writable files.\n\n"+
			"Original request:\n%s\n\n"+
			"Previous response:\n%s\n\n"+
			"Regenerate the complete project now and output ONLY files using this exact format:\n"+
			"--- FILE: path/to/file ---\n```\nfile content here\n```\n\n"+
			"Do NOT include shell commands or narrative text.",
		trimForRetryPrompt(originalPrompt, 4000),
		trimForRetryPrompt(previousOutput, 3000),
	)

	return callChatBestWithTimeout(
		ctx,
		router,
		3*time.Minute,
		model.PhaseCode,
		[]model.Message{{Role: "user", Content: retryPrompt}},
		codeSystem,
	)
}

func callChatBestWithTimeout(
	baseCtx context.Context,
	router *model.Router,
	timeout time.Duration,
	phase model.BuildPhase,
	messages []model.Message,
	system string,
	exclude ...string,
) (string, model.Usage, model.RouteResult, error) {
	ctx, cancel := context.WithTimeout(baseCtx, timeout)
	defer cancel()
	return router.ChatBest(ctx, phase, messages, system, exclude...)
}

func trimForRetryPrompt(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 32 {
		return s[:max]
	}
	return s[:max-16] + "\n...[truncated]"
}
