package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/diag"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
	"github.com/makewand/makewand/internal/tui"
	"github.com/spf13/cobra"
)

var (
	version         = "0.1.10"
	debugFlag       bool
	rootModeFlag    string
	rootPrintFlag   bool
	rootTimeoutFlag time.Duration
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "makewand [prompt]",
		Short: "Multi-provider coding router for terminal makers",
		// Provider/runtime errors are already specific; avoid noisy global usage spam.
		SilenceUsage: true,
		Long: `makewand is a multi-provider coding router that orchestrates
Claude, Gemini, and Codex through adaptive mode-based routing
(fast/balanced/power) for terminal-based coding workflows.

  makewand         - Start interactive chat in current directory (type /help in chat)
  makewand "..."   - Start chat and send an initial prompt
  makewand --print "..." - Run one prompt and print the result (CI/headless)
  makewand new     - Create a new project with guided wizard
  makewand chat    - Chat with AI about your project
  makewand preview - Start a preview server
  makewand setup   - Configure AI providers and routing preferences`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()

			if !cfg.HasAnyModel() {
				fmt.Println("No AI models configured. Run 'makewand setup' first.")
				return nil
			}

			if rootModeFlag != "" {
				if _, ok := model.ParseUsageMode(rootModeFlag); !ok {
					return fmt.Errorf("invalid mode %q: must be fast, balanced, or power", rootModeFlag)
				}
				cfg.UsageMode = rootModeFlag
			}

			initialPrompt := strings.TrimSpace(strings.Join(args, " "))
			if shouldUseHeadless(initialPrompt, rootPrintFlag, isInteractiveTerminal()) {
				return runSinglePrompt(cfg, initialPrompt, rootTimeoutFlag, debugFlag)
			}
			if initialPrompt == "" && !isInteractiveTerminal() {
				return fmt.Errorf("interactive TTY not detected; provide a prompt or use --print")
			}

			return tui.RunWithPrompt(tui.ModeChat, cfg, ".", initialPrompt, debugFlag)
		},
		Version: version,
	}

	rootCmd.AddCommand(newCmd())
	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(previewCmd())
	rootCmd.AddCommand(setupCmd())
	rootCmd.AddCommand(doctorCmd())
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable routing debug trace logging to ~/.config/makewand/trace.jsonl")
	rootCmd.Flags().StringVar(&rootModeFlag, "mode", "", "usage mode: fast, balanced, power")
	rootCmd.Flags().BoolVar(&rootPrintFlag, "print", false, "run one prompt and print the result (non-interactive)")
	rootCmd.Flags().DurationVar(&rootTimeoutFlag, "timeout", 4*time.Minute, "timeout for --print one-shot execution")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newCmd() *cobra.Command {
	var modeFlag string

	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new project with guided wizard",
		Long:  "Start the interactive wizard to create a new project from templates or your own description.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()

			if !cfg.HasAnyModel() {
				fmt.Println("Welcome to makewand!")
				fmt.Println()
				fmt.Println("No AI models found. Install a CLI tool or set an API key:")
				fmt.Println()
				fmt.Println("  Option 1: Install Claude Code, Gemini CLI, or Codex CLI (subscription)")
				fmt.Println("  Option 2: Set API keys:")
				fmt.Println("    export ANTHROPIC_API_KEY=sk-ant-...")
				fmt.Println("    export GEMINI_API_KEY=AI...")
				fmt.Println()
				fmt.Println("Run 'makewand setup' to check your configuration.")
				return nil
			}

			if modeFlag != "" {
				if _, ok := model.ParseUsageMode(modeFlag); !ok {
					return fmt.Errorf("invalid mode %q: must be fast, balanced, or power", modeFlag)
				}
				cfg.UsageMode = modeFlag
			}

			return tui.Run(tui.ModeNew, cfg, "", debugFlag)
		},
	}

	cmd.Flags().StringVar(&modeFlag, "mode", "", "usage mode: fast, balanced, power")
	return cmd
}

func chatCmd() *cobra.Command {
	var modeFlag string

	cmd := &cobra.Command{
		Use:   "chat [path]",
		Short: "Chat with AI about your project",
		Long:  "Open an interactive chat to modify and improve your project using natural language.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()

			if !cfg.HasAnyModel() {
				fmt.Println("No AI models configured. Run 'makewand setup' first.")
				return nil
			}

			if modeFlag != "" {
				if _, ok := model.ParseUsageMode(modeFlag); !ok {
					return fmt.Errorf("invalid mode %q: must be fast, balanced, or power", modeFlag)
				}
				cfg.UsageMode = modeFlag
			}

			projectPath := "."
			if len(args) > 0 {
				projectPath = args[0]
			}

			return tui.Run(tui.ModeChat, cfg, projectPath, debugFlag)
		},
	}

	cmd.Flags().StringVar(&modeFlag, "mode", "", "usage mode: fast, balanced, power")
	return cmd
}

func previewCmd() *cobra.Command {
	var allowProjectScripts bool

	cmd := &cobra.Command{
		Use:   "preview [path]",
		Short: "Start a preview server for your project",
		Long:  "Automatically detect your project type and start a development server.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectPath := "."
			if len(args) > 0 {
				projectPath = args[0]
			}

			proj, err := engine.OpenProject(projectPath)
			if err != nil {
				return fmt.Errorf("could not open project: %w", err)
			}

			fmt.Printf("Starting preview server for %s...\n", proj.Name)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			server, err := proj.StartPreview(ctx, allowProjectScripts)
			if err != nil {
				return fmt.Errorf("could not start preview: %w", err)
			}
			defer server.Stop()

			fmt.Printf("Preview running at %s\n", server.URL())
			fmt.Println("   Press Ctrl+C to stop")

			<-ctx.Done()
			return nil
		},
	}

	cmd.Flags().BoolVar(&allowProjectScripts, "allow-project-scripts", false, "allow executing project-defined scripts for preview (unsafe)")
	return cmd
}

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Configure AI models and preferences",
		Long:  "Interactive setup wizard for configuring API keys, default models, and language preferences.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()

			fmt.Println("makewand setup")
			fmt.Println()

			// Show detected CLI tools (subscription)
			if len(cfg.CLIs) > 0 {
				fmt.Println("Subscription CLI tools (auto-detected):")
				for _, cli := range cfg.CLIs {
					fmt.Printf("  [x] %s (%s) -> %s\n", cli.Name, cli.Version, cli.BinPath)
				}
				fmt.Println()
			}
			if len(cfg.CustomProviders) > 0 {
				fmt.Println("Custom providers (config-defined):")
				for _, cp := range cfg.CustomProviders {
					access := accessDisplay(cp.Access, "subscription")
					prompt := customProviderPromptLabel(cp)
					if config.IsCustomProviderUsable(cp) {
						fmt.Printf("  [x] %s -> %s (access: %s, prompt: %s)\n", cp.Name, cp.Command, access, prompt)
					} else {
						fmt.Printf("  [!] %s -> %s (access: %s, prompt: %s, unavailable)\n", cp.Name, cp.Command, access, prompt)
					}
					if warning := customProviderSafetyWarning(cp); warning != "" {
						fmt.Printf("      warning: %s\n", warning)
					}
				}
				fmt.Println()
			}

			// Show API key status
			fmt.Println("API keys:")
			if cfg.ClaudeAPIKey != "" {
				fmt.Println("  [x] Claude API key configured")
			} else if !cfg.HasCLI("claude") {
				fmt.Println("  [ ] Claude: not configured")
			}
			if cfg.GeminiAPIKey != "" {
				fmt.Println("  [x] Gemini API key configured")
			} else if !cfg.HasCLI("gemini") {
				fmt.Println("  [ ] Gemini: not configured")
			}
			if cfg.OpenAIAPIKey != "" {
				fmt.Println("  [x] OpenAI API key configured")
			} else if !cfg.HasCLI("codex") {
				fmt.Println("  [ ] OpenAI: not configured")
			}
			fmt.Println()

			fmt.Printf("  Language: %s\n", cfg.Language)
			fmt.Printf("  Default model: %s\n", cfg.DefaultModel)
			if cfg.UsageMode != "" {
				fmt.Printf("  Usage mode: %s\n", cfg.UsageMode)
			} else {
				fmt.Println("  Usage mode: not set (legacy routing)")
			}
			fmt.Println()
			fmt.Println("Provider access types:")
			fmt.Printf("  Claude: %s\n", accessDisplay(cfg.ClaudeAccess, "subscription"))
			fmt.Printf("  Gemini: %s\n", accessDisplay(cfg.GeminiAccess, "subscription"))
			fmt.Printf("  Codex:  %s\n", accessDisplay(cfg.CodexAccess, "subscription"))

			if len(cfg.CLIs) > 0 {
				fmt.Println()
				fmt.Println("Subscription CLIs detected - these will be preferred over API keys.")
				fmt.Println("No API keys needed if you have active subscriptions.")
			} else {
				fmt.Println()
				fmt.Println("No subscription CLIs detected. To use API keys:")
				fmt.Println("  export ANTHROPIC_API_KEY=sk-ant-...")
				fmt.Println("  export GEMINI_API_KEY=AI...")
				fmt.Println("  export OPENAI_API_KEY=sk-...")
			}
			fmt.Println()
			configPath, pathErr := config.ConfigPath()
			if pathErr != nil {
				fmt.Printf("Config file: <unavailable: %v>\n", pathErr)
			} else {
				fmt.Printf("Config file: %s\n", configPath)
			}

			if err := config.Save(cfg); err != nil {
				diag.Stderr().WarnErr("could not save config", err)
				fmt.Fprintln(os.Stderr, "Tip: set MAKEWAND_CONFIG_DIR to a writable directory.")
			}

			return nil
		},
	}
}

func accessDisplay(configured, defaultValue string) string {
	if configured != "" {
		return configured
	}
	return defaultValue + " (default)"
}

func loadConfigWithWarning() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		diag.Stderr().WarnErr("could not load config", err)
	}
	return cfg
}

func shouldUseHeadless(prompt string, printFlag bool, interactiveTTY bool) bool {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return false
	}
	return printFlag || !interactiveTTY
}

func isInteractiveTerminal() bool {
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func runSinglePrompt(cfg *config.Config, prompt string, timeout time.Duration, debug bool) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt is empty")
	}

	router := model.NewRouter(cfg)
	if debug {
		traceSink, tracePath, traceErr := newHeadlessTraceSink()
		if traceErr != nil {
			diag.Stderr().WarnErr("debug trace disabled", traceErr)
		} else {
			router.SetTraceSink(traceSink)
			defer traceSink.Close()
			diag.Stderr().InfoPath("Debug trace enabled", tracePath)
		}
	}
	configDir, dirErr := config.ConfigDir()
	if dirErr == nil {
		_ = router.LoadStats(configDir)
	}
	task := classifyPromptTask(prompt)
	messages := []model.Message{{Role: "user", Content: prompt}}
	systemPrompt := buildHeadlessSystemPrompt(task, prompt)

	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	var (
		content string
		usage   model.Usage
		route   model.RouteResult
		err     error
	)
	if router.ModeSet() && router.Mode() == model.ModePower {
		content, usage, route, err = router.ChatBest(ctx, promptTaskToBuildPhase(task), messages, systemPrompt)
	} else {
		content, usage, route, err = router.Chat(ctx, task, messages, systemPrompt)
	}
	if err != nil {
		return err
	}
	if dirErr == nil {
		_ = router.SaveStats(configDir)
	}

	provider := strings.TrimSpace(route.Actual)
	if provider == "" {
		provider = strings.TrimSpace(usage.Provider)
	}
	modelID := strings.TrimSpace(route.ModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(usage.Model)
	}
	if provider != "" {
		if modelID != "" {
			fmt.Fprintf(os.Stderr, "[makewand] provider=%s model=%s\n", provider, modelID)
		} else {
			fmt.Fprintf(os.Stderr, "[makewand] provider=%s\n", provider)
		}
	}

	content = sanitizeHeadlessContent(prompt, task, content)
	content = preserveGoPackageFromWorkspace(prompt, content)
	if headlessCodeOnlyRequested(task, prompt) && strings.TrimSpace(content) == "" {
		return fmt.Errorf("provider returned no usable code output in headless mode")
	}

	fmt.Println(strings.TrimSpace(content))
	return nil
}

func newHeadlessTraceSink() (*diag.JSONLTraceSink, string, error) {
	var candidates []string
	if dir, err := config.ConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "trace.jsonl"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "makewand-trace.jsonl"))
	return diag.OpenFirstJSONLTraceSink(candidates)
}

func classifyPromptTask(input string) model.TaskType {
	return model.ClassifyTask(input)
}

func promptTaskToBuildPhase(task model.TaskType) model.BuildPhase {
	switch task {
	case model.TaskCode:
		return model.PhaseCode
	case model.TaskReview:
		return model.PhaseReview
	case model.TaskFix:
		return model.PhaseFix
	default:
		return model.PhasePlan
	}
}

func buildHeadlessSystemPrompt(task model.TaskType, prompt string) string {
	base := "You are makewand, a multi-provider coding router. Provide direct, actionable answers."
	headlessRules := "Headless mode rules: do not ask for permissions, do not claim to write files, and do not ask follow-up questions. Return the final answer directly."
	if headlessCodeOnlyRequested(task, prompt) {
		return base + " " + headlessRules + " For code/file requests, output only the final code content. No markdown fences. No summaries."
	}
	return base + " " + headlessRules
}

func headlessCodeOnlyRequested(task model.TaskType, prompt string) bool {
	if task == model.TaskCode || task == model.TaskFix {
		return true
	}
	lower := strings.ToLower(prompt)
	hints := []string{
		"return only",
		"output only",
		"complete content",
		"do not output markdown",
		"no markdown",
		"no explanations",
		"--- file:",
	}
	score := 0
	for _, h := range hints {
		if strings.Contains(lower, h) {
			score++
		}
	}
	return score >= 2
}

func sanitizeHeadlessContent(prompt string, task model.TaskType, content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return content
	}
	if !headlessCodeOnlyRequested(task, prompt) {
		return content
	}

	if extracted, ok := extractFirstFileBlock(content); ok {
		return strings.TrimSpace(extracted)
	}
	if extracted, ok := extractFirstCodeFence(content); ok {
		return strings.TrimSpace(extracted)
	}

	trimmed := stripLeadingNonCode(content)
	return strings.TrimSpace(trimmed)
}

var goFileRefPattern = regexp.MustCompile(`(?i)\b([A-Za-z0-9_./-]+\.go)\b`)

func preserveGoPackageFromWorkspace(prompt, content string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	lowerPrompt := strings.ToLower(prompt)
	if !strings.Contains(lowerPrompt, "do not change package") &&
		!strings.Contains(lowerPrompt, "don't change package") &&
		!strings.Contains(lowerPrompt, "keep package name") {
		return content
	}

	m := goFileRefPattern.FindStringSubmatch(prompt)
	if len(m) < 2 {
		return content
	}
	targetPath := strings.TrimSpace(m[1])
	if targetPath == "" {
		return content
	}

	expectedPkg, ok := readGoPackageFromFile(targetPath)
	if !ok {
		return content
	}
	actualPkg, lineIdx, ok := parseGoPackageLine(content)
	if !ok || lineIdx < 0 {
		return content
	}
	if actualPkg == expectedPkg {
		return content
	}

	lines := strings.Split(content, "\n")
	lines[lineIdx] = "package " + expectedPkg
	return strings.Join(lines, "\n")
}

func readGoPackageFromFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	pkg, _, ok := parseGoPackageLine(string(data))
	return pkg, ok
}

func parseGoPackageLine(content string) (pkg string, lineIdx int, ok bool) {
	lines := strings.Split(content, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "package ") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "package "))
			if name == "" {
				return "", -1, false
			}
			name = strings.Fields(name)[0]
			return name, i, true
		}
		// First non-empty, non-comment line is not a package line.
		return "", -1, false
	}
	return "", -1, false
}

func extractFirstFileBlock(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--- FILE:") {
			start = i + 1
			break
		}
	}
	if start < 0 || start >= len(lines) {
		return "", false
	}

	inFence := false
	var out []string
	for _, line := range lines[start:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--- FILE:") {
			break
		}
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.TrimSpace(line) == "" {
			if len(out) == 0 {
				continue
			}
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, "\n"), true
}

func extractFirstCodeFence(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}

	var out []string
	for _, line := range lines[start:] {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			break
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, "\n"), true
}

func stripLeadingNonCode(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if looksLikeCodeLine(line) {
			return strings.Join(lines[i:], "\n")
		}
	}
	return content
}

func looksLikeCodeLine(line string) bool {
	l := strings.TrimSpace(strings.ToLower(line))
	if l == "" {
		return false
	}
	switch {
	case strings.HasPrefix(l, "package "),
		strings.HasPrefix(l, "import "),
		strings.HasPrefix(l, "func "),
		strings.HasPrefix(l, "type "),
		strings.HasPrefix(l, "var "),
		strings.HasPrefix(l, "const "),
		strings.HasPrefix(l, "class "),
		strings.HasPrefix(l, "function "),
		strings.HasPrefix(l, "def "),
		strings.HasPrefix(l, "from "),
		strings.HasPrefix(l, "export "),
		strings.HasPrefix(l, "module.exports"),
		strings.HasPrefix(l, "#!/"),
		strings.HasPrefix(l, "if "),
		strings.HasPrefix(l, "for "),
		strings.HasPrefix(l, "while "),
		strings.HasPrefix(l, "return "),
		strings.HasPrefix(l, "{"),
		strings.HasPrefix(l, "}"):
		return true
	}
	return strings.Contains(l, "=>") || strings.Contains(l, ";") || strings.Contains(l, "{") || strings.Contains(l, "}")
}
