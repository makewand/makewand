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

const testPrompt = "Write a Go function that reverses a string. Only output the code, no explanation."

var modes = []model.UsageMode{
	model.ModeFree,
	model.ModeEconomy,
	model.ModeBalanced,
	model.ModePower,
}

var modeNames = map[model.UsageMode]string{
	model.ModeFree:     "Free    (免费)",
	model.ModeEconomy:  "Economy (经济)",
	model.ModeBalanced: "Balanced(平衡)",
	model.ModePower:    "Power   (强劲)",
}

var tasks = []model.TaskType{
	model.TaskAnalyze,
	model.TaskCode,
	model.TaskReview,
}

var taskNames = map[model.TaskType]string{
	model.TaskAnalyze: "Analyze",
	model.TaskCode:    "Code",
	model.TaskReview:  "Review",
}

var requestTimeout = flag.Duration("timeout", 45*time.Second, "per-request timeout for live provider calls")

func main() {
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		diag.Stderr().ErrorErr("config load failed", err)
		os.Exit(1)
	}

	// ── Phase 1: Routing Decision Table ──────────────────────────
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║              makewand 四种模式路由对比测试                              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("Request timeout: %s\n\n", requestTimeout.String())

	fmt.Println("▸ 可用 Provider:")
	tmpRouter := model.NewRouter(cfg)
	availableProviders := tmpRouter.Available()
	for _, name := range availableProviders {
		fmt.Printf("  ✓ %s\n", name)
	}
	if len(availableProviders) == 0 {
		fmt.Println("  (none)")
	}
	fmt.Println()

	fmt.Println("═══ 1. 路由决策表 (每种模式 × 任务 → 选择的 provider + model) ═══")
	fmt.Println()

	header := fmt.Sprintf("  %-20s", "模式 \\ 任务")
	for _, t := range tasks {
		header += fmt.Sprintf("│ %-28s", taskNames[t])
	}
	fmt.Println(header)
	fmt.Println("  " + strings.Repeat("─", 20) + "┼" + strings.Repeat("─", 28) + "┼" + strings.Repeat("─", 28) + "┼" + strings.Repeat("─", 28))

	for _, mode := range modes {
		cfg.UsageMode = mode.String()
		router := model.NewRouter(cfg)

		row := fmt.Sprintf("  %-20s", modeNames[mode])
		for _, t := range tasks {
			result, err := router.Route(t)
			if err != nil {
				row += fmt.Sprintf("│ %-28s", "✗ "+truncate(err.Error(), 20))
			} else {
				cell := result.Actual
				if result.ModelID != "" {
					cell = result.Actual + "/" + shortModel(result.ModelID)
				}
				if result.IsFallback {
					cell += " ↩"
				}
				row += fmt.Sprintf("│ %-28s", cell)
			}
		}
		fmt.Println(row)
	}
	fmt.Println()

	// ── Phase 2: Live Test ───────────────────────────────────────
	fmt.Println("═══ 2. 实际调用测试 (同一 prompt, 4 种模式对比) ═══")
	fmt.Printf("  Prompt: %s\n\n", testPrompt)

	type result struct {
		mode     string
		provider string
		modelID  string
		elapsed  time.Duration
		output   string
		cost     float64
		tokens   int
		err      error
	}

	var results []result

	for _, mode := range modes {
		cfg.UsageMode = mode.String()
		router := model.NewRouter(cfg)

		messages := []model.Message{
			{Role: "user", Content: testPrompt},
		}

		mn := modeNames[mode]
		fmt.Printf("  ▸ 测试模式: %s ...", mn)

		ctx, cancel := context.WithTimeout(context.Background(), *requestTimeout)
		start := time.Now()
		content, usage, rr, err := router.Chat(
			ctx,
			model.TaskCode,
			messages,
			"You are a Go programming expert. Be concise.",
		)
		elapsed := time.Since(start)
		cancel()

		r := result{
			mode:    mn,
			elapsed: elapsed,
		}

		if err != nil {
			r.err = err
			fmt.Printf(" ✗ %v\n", err)
		} else {
			r.provider = rr.Actual
			r.modelID = rr.ModelID
			r.output = content
			r.cost = usage.Cost
			r.tokens = usage.InputTokens + usage.OutputTokens
			fmt.Printf(" ✓ %s (%.1fs)\n", r.provider+"/"+shortModel(r.modelID), elapsed.Seconds())
		}

		results = append(results, r)
	}

	// ── Phase 3: Summary ─────────────────────────────────────────
	fmt.Println()
	fmt.Println("═══ 3. 对比结果总结 ═══")
	fmt.Println()

	fmt.Printf("  %-20s │ %-28s │ %8s │ %7s │ %s\n",
		"模式", "Provider/Model", "耗时", "费用", "Tokens")
	fmt.Println("  " + strings.Repeat("─", 20) + "─┼─" + strings.Repeat("─", 28) + "─┼─" + strings.Repeat("─", 8) + "─┼─" + strings.Repeat("─", 7) + "─┼─" + strings.Repeat("─", 8))

	for _, r := range results {
		if r.err != nil {
			fmt.Printf("  %-20s │ %-28s │ %8s │ %7s │ %s\n",
				r.mode, "ERROR", "-", "-", r.err.Error())
		} else {
			pm := r.provider + "/" + shortModel(r.modelID)
			costStr := "free"
			if r.cost > 0 {
				costStr = fmt.Sprintf("$%.4f", r.cost)
			}
			fmt.Printf("  %-20s │ %-28s │ %7.1fs │ %7s │ %d\n",
				r.mode, pm, r.elapsed.Seconds(), costStr, r.tokens)
		}
	}

	// ── Phase 4: Output Samples ──────────────────────────────────
	fmt.Println()
	fmt.Println("═══ 4. 各模式输出内容 ═══")

	for _, r := range results {
		fmt.Println()
		fmt.Printf("── %s ──\n", r.mode)
		if r.err != nil {
			fmt.Printf("  Error: %v\n", r.err)
		} else {
			// Truncate long output
			out := strings.TrimSpace(r.output)
			lines := strings.Split(out, "\n")
			if len(lines) > 30 {
				lines = append(lines[:30], "  ... (truncated)")
			}
			for _, line := range lines {
				fmt.Printf("  %s\n", line)
			}
		}
	}

	fmt.Println()
	fmt.Println("═══ 测试完成 ═══")
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max]
}

func shortModel(id string) string {
	// Shorten model IDs for display
	parts := strings.Split(id, "-")
	if len(parts) > 3 {
		return strings.Join(parts[:3], "-")
	}
	return id
}
