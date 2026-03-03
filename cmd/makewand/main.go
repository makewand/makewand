package main

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/tui"
	"github.com/spf13/cobra"
)

var (
	version   = "0.1.0"
	debugFlag bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "makewand",
		Short: "AI coding assistant for everyone",
		Long: `makewand is a terminal AI coding assistant that lets anyone
build, modify, and deploy software through natural language conversation.

  makewand new     - Create a new project with guided wizard
  makewand chat    - Chat with AI about your project
  makewand preview - Start a preview server
  makewand setup   - Configure AI models and preferences`,
		Version: version,
	}

	rootCmd.AddCommand(newCmd())
	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(previewCmd())
	rootCmd.AddCommand(setupCmd())
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable routing debug trace logging to ~/.config/makewand/trace.jsonl")

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
			cfg, err := config.Load()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not load config: %v\n", err)
			}

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
				cfg.UsageMode = modeFlag
			}

			return tui.Run(tui.ModeNew, cfg, "", debugFlag)
		},
	}

	cmd.Flags().StringVar(&modeFlag, "mode", "", "usage mode: free, economy, balanced, power")
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
			cfg, err := config.Load()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not load config: %v\n", err)
			}

			if !cfg.HasAnyModel() {
				fmt.Println("No AI models configured. Run 'makewand setup' first.")
				return nil
			}

			if modeFlag != "" {
				cfg.UsageMode = modeFlag
			}

			projectPath := "."
			if len(args) > 0 {
				projectPath = args[0]
			}

			return tui.Run(tui.ModeChat, cfg, projectPath, debugFlag)
		},
	}

	cmd.Flags().StringVar(&modeFlag, "mode", "", "usage mode: free, economy, balanced, power")
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
			cfg, err := config.Load()
			if err != nil {
				cfg = config.DefaultConfig()
			}

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
			if cfg.OllamaURL != "" {
				fmt.Printf("  Ollama URL: %s\n", cfg.OllamaURL)
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
			fmt.Printf("  Claude: %s\n", accessDisplay(cfg.ClaudeAccess, "api"))
			fmt.Printf("  Gemini: %s\n", accessDisplay(cfg.GeminiAccess, "free"))
			fmt.Printf("  Codex:  %s\n", accessDisplay(cfg.CodexAccess, "api"))
			fmt.Printf("  Ollama: %s\n", accessDisplay(cfg.OllamaAccess, "local"))

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
				fmt.Fprintf(os.Stderr, "Warning: could not save config: %v\n", err)
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
