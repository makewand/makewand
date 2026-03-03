package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

// Head-to-head: raw CLI tools vs makewand multi-model pipeline.
// Same prompt, same machine, fair comparison.

const prompt = `Create a password strength checker web app with:
1. An input field for the password
2. Real-time strength indicator (weak/medium/strong/very strong)
3. Show which criteria are met: length>=8, uppercase, lowercase, digit, special char
4. Color-coded visual feedback

Output each file with:
--- FILE: path/to/file ---
` + "```" + `
content
` + "```" + `

Generate ALL files needed. Use plain HTML/CSS/JS (no frameworks). Keep it minimal and working.`

const systemPrompt = "You are an expert programmer. Generate complete, working code. Be concise."

type result struct {
	name       string
	elapsed    time.Duration
	output     string
	files      []engine.ExtractedFile
	totalLines int
	err        error
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║   Raw CLI vs makewand 多模型管道对比 (versus)                       ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║   同一 prompt → gemini / codex / claude / makewand balanced         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Show versions
	showVersion("claude", "--version")
	showVersion("gemini", "--version")
	showVersion("codex", "--version")
	fmt.Println()

	fullPrompt := systemPrompt + "\n\n" + prompt

	var results []result

	// 1. Gemini CLI raw
	fmt.Println("━━━ [1/4] gemini CLI (raw) ━━━")
	results = append(results, runCLI("gemini (raw)", "gemini", []string{"-p", fullPrompt, "--sandbox", "false"}))

	// 2. Codex CLI raw
	fmt.Println("━━━ [2/4] codex CLI (raw) ━━━")
	results = append(results, runCLI("codex (raw)", "codex", []string{"exec", "--skip-git-repo-check", fullPrompt}))

	// 3. Claude CLI raw
	fmt.Println("━━━ [3/4] claude CLI (raw) ━━━")
	results = append(results, runCLI("claude (raw)", "claude", []string{"-p", fullPrompt}))

	// 4. makewand balanced pipeline
	fmt.Println("━━━ [4/4] makewand balanced (multi-model) ━━━")
	results = append(results, runMakewand())

	// Summary table
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                              对比结果                                      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	fmt.Printf("  %-24s │ %6s │ %5s │ %6s │ %s\n",
		"Tool", "Time", "Files", "Lines", "Files Generated")
	fmt.Println("  " + strings.Repeat("─", 24) + "─┼─" + strings.Repeat("─", 6) +
		"─┼─" + strings.Repeat("─", 5) + "─┼─" + strings.Repeat("─", 6) + "─┼─" + strings.Repeat("─", 40))

	for _, r := range results {
		if r.err != nil {
			fmt.Printf("  %-24s │ %6s │ %5s │ %6s │ %s\n",
				r.name, "-", "-", "-", "ERROR: "+r.err.Error())
			continue
		}

		var fileNames []string
		for _, f := range r.files {
			fileNames = append(fileNames, f.Path)
		}
		filesStr := strings.Join(fileNames, ", ")
		if len(filesStr) > 40 {
			filesStr = filesStr[:37] + "..."
		}

		fmt.Printf("  %-24s │ %5.0fs │ %5d │ %6d │ %s\n",
			r.name, r.elapsed.Seconds(), len(r.files), r.totalLines, filesStr)
	}

	// Code detail comparison
	fmt.Println()
	fmt.Println("═══ 各工具主文件对比 (index.html 前 40 行) ═══")

	for _, r := range results {
		if r.err != nil || len(r.files) == 0 {
			continue
		}
		var mainFile engine.ExtractedFile
		for _, f := range r.files {
			if strings.HasSuffix(f.Path, ".html") || strings.Contains(f.Path, "index") {
				mainFile = f
				break
			}
		}
		if mainFile.Path == "" {
			mainFile = r.files[0]
		}

		totalLines := strings.Count(mainFile.Content, "\n") + 1
		fmt.Printf("\n── %s → %s (%d lines) ──\n", r.name, mainFile.Path, totalLines)
		lines := strings.Split(mainFile.Content, "\n")
		for i, line := range lines {
			if i >= 40 {
				fmt.Printf("  ... (+%d more lines)\n", len(lines)-40)
				break
			}
			fmt.Printf("  %s\n", line)
		}
	}

	// Feature comparison
	fmt.Println()
	fmt.Println("═══ 功能检查 ═══")
	fmt.Println()

	checks := []struct {
		name    string
		pattern string
	}{
		{"Password input field", "<input"},
		{"Strength levels", "strong"},
		{"Length check (>=8)", "length"},
		{"Uppercase check", "UpperCase"},
		{"Lowercase check", "LowerCase"},
		{"Digit check", "\\d"},
		{"Special char check", "special"},
		{"Color feedback", "color"},
		{"Real-time (oninput/addEventListener)", "input"},
		{"Toggle visibility", "visibility"},
	}

	fmt.Printf("  %-24s", "Feature")
	for _, r := range results {
		name := r.name
		if len(name) > 12 {
			name = name[:12]
		}
		fmt.Printf(" │ %-12s", name)
	}
	fmt.Println()
	fmt.Print("  " + strings.Repeat("─", 24))
	for range results {
		fmt.Print("─┼─" + strings.Repeat("─", 12))
	}
	fmt.Println()

	for _, check := range checks {
		fmt.Printf("  %-24s", check.name)
		for _, r := range results {
			if r.err != nil {
				fmt.Printf(" │ %-12s", "—")
				continue
			}
			found := false
			lower := strings.ToLower(r.output)
			searchTerm := strings.ToLower(check.pattern)
			if strings.Contains(lower, searchTerm) {
				found = true
			}
			if found {
				fmt.Printf(" │ %-12s", "✓")
			} else {
				fmt.Printf(" │ %-12s", "✗")
			}
		}
		fmt.Println()
	}

	// Architecture comparison
	fmt.Println()
	fmt.Println("═══ 架构对比 ═══")
	fmt.Println()

	for _, r := range results {
		if r.err != nil {
			continue
		}
		fmt.Printf("  %s:\n", r.name)
		for _, f := range r.files {
			lines := strings.Count(f.Content, "\n") + 1
			fmt.Printf("    %-30s  %4d lines\n", f.Path, lines)
		}
		fmt.Printf("    Total: %d files, %d lines\n\n", len(r.files), r.totalLines)
	}

	fmt.Println("═══ 测试完成 ═══")
}

func runCLI(name, bin string, args []string) result {
	r := result{name: name}

	binPath, err := exec.LookPath(bin)
	if err != nil {
		r.err = fmt.Errorf("%s not found", bin)
		fmt.Printf("  ✗ %s not found\n\n", bin)
		return r
	}

	fmt.Printf("  Running %s... ", binPath)

	timeout := 5 * time.Minute
	if bin == "codex" {
		timeout = 90 * time.Second // codex exec can block; hard limit
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.Command(binPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Claude Code: unset CLAUDECODE for nested invocation
	if bin == "claude" {
		env := os.Environ()
		filtered := make([]string, 0, len(env))
		for _, e := range env {
			if !strings.HasPrefix(e, "CLAUDECODE=") {
				filtered = append(filtered, e)
			}
		}
		cmd.Env = filtered
	}

	// Gemini: bypass proxy
	if bin == "gemini" {
		cmd.Env = ensureNoProxyCopy(cmd.Environ())
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		r.err = fmt.Errorf("start: %v", err)
		fmt.Printf("✗ %v\n\n", err)
		return r
	}

	// Kill entire process group on context cancel
	cmdDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-cmdDone:
		}
	}()

	start := time.Now()
	err = cmd.Wait()
	close(cmdDone)
	r.elapsed = time.Since(start)

	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		r.err = fmt.Errorf("%s", errMsg)
		fmt.Printf("✗ (%.0fs) %s\n\n", r.elapsed.Seconds(), errMsg[:min(len(errMsg), 100)])
		return r
	}

	r.output = stripANSI(stdout.String())
	parsed := engine.ParseFiles(r.output)
	r.files = parsed.Files

	for _, f := range r.files {
		r.totalLines += strings.Count(f.Content, "\n") + 1
	}

	fmt.Printf("✓ %.0fs, %d files, %d lines\n\n", r.elapsed.Seconds(), len(r.files), r.totalLines)
	return r
}

func runMakewand() result {
	r := result{name: "makewand (balanced)"}

	cfg, err := config.Load()
	if err != nil {
		r.err = err
		fmt.Printf("  ✗ config: %v\n\n", err)
		return r
	}

	cfg.UsageMode = "balanced"
	router := model.NewRouter(cfg)
	ctx := context.Background()

	// Phase 1: Code (claude)
	codeProvider := router.BuildProviderFor(model.PhaseCode)
	fmt.Printf("  Code: %s... ", codeProvider)

	start := time.Now()
	codeContent, _, codeResult, err := router.ChatWith(ctx, codeProvider, model.PhaseCode,
		[]model.Message{{Role: "user", Content: prompt}}, systemPrompt)
	codeElapsed := time.Since(start)

	if err != nil {
		r.err = err
		fmt.Printf("✗ %v\n\n", err)
		return r
	}
	fmt.Printf("✓ %.0fs (%s)\n", codeElapsed.Seconds(), codeResult.Actual)

	// Phase 2: Review (gemini — cross-model)
	reviewProvider := router.BuildProviderFor(model.PhaseReview)
	if reviewProvider == codeResult.Actual {
		for _, fb := range []string{"gemini", "codex", "ollama", "claude"} {
			if fb != codeResult.Actual {
				reviewProvider = fb
				break
			}
		}
	}
	fmt.Printf("  Review: %s... ", reviewProvider)

	reviewPrompt := fmt.Sprintf(
		"Review this code for bugs and issues. If issues exist, output corrected files using:\n--- FILE: path ---\n```\ncontent\n```\n\nIf code is fine, respond with: LGTM\n\n%s", codeContent)

	reviewStart := time.Now()
	reviewContent, _, reviewResult, err := router.ChatWith(ctx, reviewProvider, model.PhaseReview,
		[]model.Message{{Role: "user", Content: reviewPrompt}},
		"You are an expert code reviewer. Only fix real bugs, not style. Be concise.")
	reviewElapsed := time.Since(reviewStart)

	if err != nil {
		fmt.Printf("✗ (skipped: %v)\n", err)
		// Review is non-fatal, use code output as-is
		r.output = codeContent
	} else {
		hasIssues := !isLGTMResponse(reviewContent)
		if hasIssues {
			reviewParsed := engine.ParseFiles(reviewContent)
			if len(reviewParsed.Files) > 0 {
				// Merge review fixes into code output
				r.output = reviewContent
				fmt.Printf("✓ %.0fs (%s) → %d fixes applied\n", reviewElapsed.Seconds(), reviewResult.Actual, len(reviewParsed.Files))
			} else {
				r.output = codeContent
				fmt.Printf("✓ %.0fs (%s) → comments only, no file changes\n", reviewElapsed.Seconds(), reviewResult.Actual)
			}
		} else {
			r.output = codeContent
			fmt.Printf("✓ %.0fs (%s) → LGTM\n", reviewElapsed.Seconds(), reviewResult.Actual)
		}
	}

	r.elapsed = time.Since(start)

	parsed := engine.ParseFiles(r.output)
	r.files = parsed.Files
	for _, f := range r.files {
		r.totalLines += strings.Count(f.Content, "\n") + 1
	}

	fmt.Printf("  Total: %.0fs, %d files, %d lines\n\n", r.elapsed.Seconds(), len(r.files), r.totalLines)
	return r
}

func showVersion(bin, flag string) {
	out, err := exec.Command(bin, flag).Output()
	if err != nil {
		fmt.Printf("  %s: not found\n", bin)
		return
	}
	ver := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	fmt.Printf("  %s: %s\n", bin, ver)
}

func stripANSI(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
					i++
				}
				if i < len(s) {
					i++
				}
			}
		} else {
			result.WriteByte(s[i])
			i++
		}
	}
	return result.String()
}

func isLGTMResponse(content string) bool {
	return strings.EqualFold(strings.TrimSpace(content), "LGTM")
}

func ensureNoProxyCopy(env []string) []string {
	const hosts = "googleapis.com,google.com,cloudcode-pa.googleapis.com"
	for i, e := range env {
		if strings.HasPrefix(e, "NO_PROXY=") || strings.HasPrefix(e, "no_proxy=") {
			if !strings.Contains(e, "googleapis.com") {
				env[i] = e + "," + hosts
			}
			return env
		}
	}
	return append(env, "NO_PROXY="+hosts, "no_proxy="+hosts)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
