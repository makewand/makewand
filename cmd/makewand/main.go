package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/tui"
)

var version = "0.1.0"

func main() {
	rootCmd := &cobra.Command{
		Use:   "makewand",
		Short: "AI coding assistant for everyone",
		Long: `makewand is a terminal AI coding assistant that lets anyone
build, modify, and deploy software through natural language conversation.

  🪄 makewand new     - Create a new project with guided wizard
  💬 makewand chat    - Chat with AI about your project
  👁️  makewand preview - Start a preview server
  ⚙️  makewand setup   - Configure AI models and preferences`,
		Version: version,
	}

	rootCmd.AddCommand(newCmd())
	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(previewCmd())
	rootCmd.AddCommand(setupCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new",
		Short: "Create a new project with guided wizard",
		Long:  "Start the interactive wizard to create a new project from templates or your own description.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not load config: %v\n", err)
			}

			if !cfg.HasAnyModel() {
				fmt.Println("🪄 Welcome to makewand!")
				fmt.Println()
				fmt.Println("No AI models configured yet. You need at least one to get started.")
				fmt.Println()
				fmt.Println("Quick setup:")
				fmt.Println("  export ANTHROPIC_API_KEY=sk-ant-...")
				fmt.Println("  export GEMINI_API_KEY=AI...")
				fmt.Println("  # or run: makewand setup")
				fmt.Println()
				return nil
			}

			return tui.Run(tui.ModeNew, cfg, "")
		},
	}
}

func chatCmd() *cobra.Command {
	return &cobra.Command{
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
				fmt.Println("🪄 No AI models configured. Run 'makewand setup' first.")
				return nil
			}

			projectPath := "."
			if len(args) > 0 {
				projectPath = args[0]
			}

			return tui.Run(tui.ModeChat, cfg, projectPath)
		},
	}
}

func previewCmd() *cobra.Command {
	return &cobra.Command{
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

			fmt.Printf("🪄 Starting preview server for %s...\n", proj.Name)

			ctx := cmd.Context()
			server, err := proj.StartPreview(ctx)
			if err != nil {
				return fmt.Errorf("could not start preview: %w", err)
			}
			defer server.Stop()

			fmt.Printf("✨ Preview running at %s\n", server.URL())
			fmt.Println("   Press Ctrl+C to stop")

			// Wait for interrupt
			<-ctx.Done()
			return nil
		},
	}
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

			fmt.Println("🪄 makewand setup")
			fmt.Println()

			// Show current config
			fmt.Println("Current configuration:")
			if cfg.ClaudeAPIKey != "" {
				fmt.Println("  ✓ Claude API key configured")
			} else {
				fmt.Println("  ✗ Claude API key not set")
			}
			if cfg.GeminiAPIKey != "" {
				fmt.Println("  ✓ Gemini API key configured")
			} else {
				fmt.Println("  ✗ Gemini API key not set")
			}
			if cfg.OpenAIAPIKey != "" {
				fmt.Println("  ✓ OpenAI API key configured")
			} else {
				fmt.Println("  ✗ OpenAI API key not set")
			}
			fmt.Printf("  Ollama URL: %s\n", cfg.OllamaURL)
			fmt.Printf("  Language: %s\n", cfg.Language)
			fmt.Printf("  Default model: %s\n", cfg.DefaultModel)

			fmt.Println()
			fmt.Println("To configure, set environment variables:")
			fmt.Println("  export ANTHROPIC_API_KEY=sk-ant-...")
			fmt.Println("  export GEMINI_API_KEY=AI...")
			fmt.Println("  export OPENAI_API_KEY=sk-...")
			fmt.Println()
			fmt.Println("Or edit: ~/.config/makewand/config.json")

			// Save current config
			if err := config.Save(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not save config: %v\n", err)
			}

			return nil
		},
	}
}
