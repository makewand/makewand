package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/buildinfo"
	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/diag"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
	"github.com/makewand/makewand/internal/tui"
	"github.com/spf13/cobra"
)

var (
	debugFlag        bool
	rootModeFlag     string
	rootPrintFlag    bool
	rootTimeoutFlag  time.Duration
	repoTrustFlag    string
	rootApprovalFlag string

	// resolvedRepoTrust holds the repository trust level parsed once by the root
	// command's PersistentPreRunE, so every subcommand shares a single validated
	// value. PersistentPreRunE rejects an invalid --repo-trust before any backend
	// check or router construction runs.
	resolvedRepoTrust model.RepoTrust

	// resolvedApprovalOverride holds the canonical --approval value parsed once
	// by PersistentPreRunE ("" when the flag was not given). It overrides the
	// configured approval mode for this process only; nothing writes it back to
	// disk except `makewand setup`, which persists its (possibly overridden)
	// config as part of its normal save.
	resolvedApprovalOverride string
)

// resolveApprovalOverride strictly parses the --approval flag value. An empty
// value means "keep the configured mode". Anything else must parse exactly;
// NormalizeApprovalMode is NOT used here because it silently degrades unknown
// values to manual, which would hide typos like --approval autopilto.
func resolveApprovalOverride(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	mode, ok := config.ParseApprovalMode(value)
	if !ok {
		return "", fmt.Errorf("invalid --approval %q: must be manual, safe, or autopilot", value)
	}
	return mode, nil
}

// resolveRepoTrust parses the --repo-trust flag value into a model.RepoTrust.
// An empty or "trusted" value resolves to the trusted default; "untrusted"
// enables fail-closed untrusted-repository routing. Any other value is rejected
// with an actionable error.
func resolveRepoTrust(value string) (model.RepoTrust, error) {
	trust, ok := model.ParseRepoTrust(value)
	if !ok {
		return model.RepoTrustTrusted, fmt.Errorf("invalid --repo-trust %q: must be \"trusted\" or \"untrusted\"", value)
	}
	return trust, nil
}

// pythonOrchestratorCmds are the subcommands implemented by the Python
// orchestrator; the Go binary forwards them unchanged.
var pythonOrchestratorCmds = map[string]bool{
	"run": true, "review": true, "race": true, "observe": true,
	"models": true, "candidates": true, "inspect": true, "apply": true,
	"discard": true, "sandbox": true, "status": true, "probe": true,
	"repomap": true, "plan": true, "aci": true, "mcp": true,
}

// pythonDelegation is a command line routed to the Python orchestrator.
type pythonDelegation struct {
	subcmd string
	args   []string // argv for the Python CLI (global flags first, then the subcommand)
	notes  []string // non-fatal notices for stderr
}

// planPythonDelegation decides whether args name a Python orchestrator
// subcommand. Root persistent flags (--repo-trust, --approval, --debug) may
// precede the subcommand, in both "--flag value" and "--flag=value" forms, as
// cobra's help advertises; they are skipped when looking for the subcommand.
// It returns (nil, nil) when the command line belongs to the Go CLI.
//
// Forwarding: --repo-trust is understood by the Python CLI and forwarded.
// --debug only controls Go-side routing traces and is dropped with a note.
// --approval cannot be honored by the Python orchestrator (it has no approval
// modes), so it is rejected rather than silently ignored - the same outcome the
// Python CLI gives when --approval follows the subcommand.
func planPythonDelegation(args []string) (*pythonDelegation, error) {
	var (
		globals  []string
		approval string
		debug    bool
		i        int
	)
	flagValue := func(name string) (string, bool, error) {
		arg := args[i]
		if arg == name {
			if i+1 >= len(args) {
				return "", true, fmt.Errorf("flag needs an argument: %s", name)
			}
			i += 2
			return args[i-1], true, nil
		}
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			i++
			return value, true, nil
		}
		return "", false, nil
	}
scan:
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--debug":
			debug = true
			i++
		case strings.HasPrefix(arg, "--debug="):
			value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--debug="))
			if err != nil {
				return nil, fmt.Errorf("invalid --debug value %q", strings.TrimPrefix(arg, "--debug="))
			}
			debug = value
			i++
		default:
			if value, ok, err := flagValue("--repo-trust"); ok {
				if err != nil {
					return nil, err
				}
				if _, err := resolveRepoTrust(value); err != nil {
					return nil, err
				}
				globals = append(globals, "--repo-trust", value)
				continue
			}
			if value, ok, err := flagValue("--approval"); ok {
				if err != nil {
					return nil, err
				}
				mode, err := resolveApprovalOverride(value)
				if err != nil {
					return nil, err
				}
				approval = mode
				continue
			}
			break scan
		}
	}
	if i >= len(args) || !pythonOrchestratorCmds[args[i]] {
		return nil, nil
	}
	subcmd := args[i]
	rest := args[i+1:]
	if approval != "" {
		return nil, fmt.Errorf("--approval %s applies to makewand chat sessions only; %q is handled by the Python orchestrator, which has no approval modes, so it cannot be honored (remove --approval)", approval, subcmd)
	}

	// Validate --repo-trust early if present in delegated arguments
	for j, arg := range rest {
		var val string
		if strings.HasPrefix(arg, "--repo-trust=") {
			val = strings.TrimPrefix(arg, "--repo-trust=")
		} else if arg == "--repo-trust" && j+1 < len(rest) {
			val = rest[j+1]
		}
		if val != "" {
			if val != "trusted" && val != "untrusted" {
				return nil, fmt.Errorf("invalid --repo-trust %q: must be \"trusted\" or \"untrusted\"", val)
			}
		}
	}

	delegation := &pythonDelegation{subcmd: subcmd}
	if debug {
		delegation.notes = append(delegation.notes, "note: --debug only affects makewand chat sessions; ignored for "+subcmd)
	}
	delegation.args = append(append(globals, subcmd), translatePythonTierArgs(rest)...)
	return delegation, nil
}

// translatePythonTierArgs normalizes --mode / --tier across the Go and Python
// boundary: Python receives --tier with canonical Python tier names.
func translatePythonTierArgs(args []string) []string {
	translatedArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "--mode" || arg == "--tier") && i+1 < len(args) {
			value := args[i+1]
			i++
			if pyTier, ok := engine.ToPythonTier(value); ok {
				value = pyTier
			}
			translatedArgs = append(translatedArgs, "--tier", value)
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--mode="); ok {
			if pyTier, ok := engine.ToPythonTier(value); ok {
				value = pyTier
			}
			translatedArgs = append(translatedArgs, "--tier="+value)
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--tier="); ok {
			if pyTier, ok := engine.ToPythonTier(value); ok {
				value = pyTier
			}
			translatedArgs = append(translatedArgs, "--tier="+value)
			continue
		}
		translatedArgs = append(translatedArgs, arg)
	}
	return translatedArgs
}

func tryDelegateToPythonOrchestrator(args []string) bool {
	delegation, err := planPythonDelegation(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if delegation == nil {
		return false
	}
	for _, note := range delegation.notes {
		fmt.Fprintln(os.Stderr, note)
	}
	subcmd := delegation.subcmd
	args = delegation.args

	pyBin, err := exec.LookPath("python3")
	if err != nil {
		pyBin, err = exec.LookPath("python")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: python3 runtime is required to execute %s\n", subcmd)
		os.Exit(1)
	}

	exePath, _ := os.Executable()
	if realExe, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = realExe
	}
	home, _ := os.UserHomeDir()
	launcher := findPythonLauncher(exePath, os.Getenv("MAKEWAND_HOME"), home)
	var cmd *exec.Cmd
	if launcher != "" {
		// Isolated mode ignores cwd, PYTHONPATH and user site startup hooks. The
		// installed launcher inserts its own fixed package root explicitly.
		//nolint:gosec // G702: executable is a fixed Python name resolved from the user's PATH; launcher is a verified installation path and arguments never pass through a shell.
		cmd = exec.Command(pyBin, append([]string{"-I", launcher}, args...)...)
	} else {
		checkCmd := exec.Command(pyBin, "-I", "-c", "import makewand")
		if err := checkCmd.Run(); err == nil {
			//nolint:gosec // G702: fixed interpreter/module, isolated import path, and literal argv forwarding without shell evaluation.
			cmd = exec.Command(pyBin, append([]string{"-I", "-m", "makewand"}, args...)...)
		} else {
			fmt.Fprintln(os.Stderr, "Error: makewand Python engine could not be located. Keep the release lib/ directory beside the binary, run scripts/install.sh, or set MAKEWAND_HOME to a complete installation.")
			os.Exit(1)
		}
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
	os.Exit(0)
	return true
}

// findPythonLauncher uses installation roots only, never the working directory
// or an arbitrary executable named makewand on PATH.
func findPythonLauncher(exePath, explicitHome, userHome string) string {
	exeDir := filepath.Dir(exePath)
	roots := []string{}
	if explicitHome != "" {
		if absolute, err := filepath.Abs(explicitHome); err == nil {
			roots = append(roots, absolute, filepath.Join(absolute, "lib", "makewand", "python"))
		}
	}
	roots = append(roots,
		filepath.Join(exeDir, "lib", "makewand", "python"), // release / Homebrew libexec
		filepath.Join(filepath.Dir(exeDir), "lib", "makewand", "python"),
		filepath.Dir(exeDir), // source checkout bin/ or build/
	)
	if userHome != "" {
		roots = append(roots, filepath.Join(userHome, ".local", "share", "makewand"), filepath.Join(userHome, ".makewand_app"))
	}
	roots = append(roots, "/usr/local/share/makewand", "/usr/share/makewand")
	for _, root := range roots {
		launcher := filepath.Join(root, "bin", "makewand")
		packageFile := filepath.Join(root, "makewand", "__init__.py")
		if info, err := os.Stat(packageFile); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info, err := os.Stat(launcher); err == nil && info.Mode().IsRegular() {
			return launcher
		}
	}
	return ""
}

func main() {
	if len(os.Args) > 1 {
		tryDelegateToPythonOrchestrator(os.Args[1:])
	}
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the makewand command tree. It is a function (not inline in
// main) so tests can exercise the persistent flag validation and command wiring
// without spawning a subprocess.
func newRootCmd() *cobra.Command {
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
  makewand serve   - Expose your configured backend for your other devices
  makewand token   - Manage remote server auth tokens
  makewand audit   - Inspect server audit logs
  makewand usage   - Inspect structured server usage logs
  makewand user    - Manage registered server users
  makewand preview - Start a preview server
  makewand setup   - Configure AI providers and routing preferences

Flags:
  --repo-trust trusted|untrusted  Repository trust level (default: trusted).
      In untrusted mode only direct API providers may generate against the repo
      (fail closed), and repo-provided .makewand/rules.md is not treated as
      trusted instructions. Use it for third-party/unreviewed repositories.
  --approval manual|safe|autopilot  Approval mode for this run only (default:
      configured value). Persist a default with 'makewand setup --approval ...'.
      autopilot generates and ranks several candidates, but it only applies one
      automatically at verification Strength 2 (an independent acceptance
      check). Local checks top out at Strength 1 because test results come from
      candidate code, so autopilot currently always asks before writing.`,
		Args: cobra.ArbitraryArgs,
		// Validate --repo-trust once, globally, before any subcommand's RunE and
		// before any backend check. This is a persistent flag, so a bad value must
		// be rejected on every subcommand (serve/doctor/setup included), not only
		// on the paths that later resolve it. The resolved value is stored for
		// reuse so commands that apply the trust do not re-parse it.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			trust, err := resolveRepoTrust(repoTrustFlag)
			if err != nil {
				return err
			}
			resolvedRepoTrust = trust
			approval, err := resolveApprovalOverride(rootApprovalFlag)
			if err != nil {
				return err
			}
			resolvedApprovalOverride = approval
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()

			if !hasUsableBackend(cfg) {
				fmt.Println("No AI models or remote backend configured. Run 'makewand setup' or set MAKEWAND_REMOTE_URL/MAKEWAND_REMOTE_TOKEN.")
				return nil
			}

			if rootModeFlag != "" {
				m, ok := model.ParseUsageMode(rootModeFlag)
				if !ok {
					return fmt.Errorf("invalid mode %q: must be fast, balanced, or power", rootModeFlag)
				}
				cfg.UsageMode = m.String()
			}

			repoTrust, trustErr := resolveRepoTrust(repoTrustFlag)
			if trustErr != nil {
				return trustErr
			}

			initialPrompt := strings.TrimSpace(strings.Join(args, " "))
			isTTY := isInteractiveTerminal()

			// Read prompt from stdin when piped and no args provided.
			if initialPrompt == "" && !isTTY {
				stdinPrompt, stdinErr := readStdinPrompt()
				if stdinErr == nil && stdinPrompt != "" {
					initialPrompt = stdinPrompt
				}
			}

			hostAuth := resolveUnsafeHostExecAuth(cfg)

			if shouldUseHeadless(initialPrompt, rootPrintFlag, isTTY) {
				return runSinglePrompt(cfg, initialPrompt, rootTimeoutFlag, repoTrust, hostAuth, debugFlag)
			}
			if rootPrintFlag && initialPrompt == "" {
				return fmt.Errorf("--print requires a non-empty prompt (via argument or piped stdin)")
			}
			if initialPrompt == "" && !isTTY {
				return fmt.Errorf("interactive TTY not detected; provide a prompt or use --print")
			}

			return tui.RunWithPrompt(tui.ModeChat, cfg, ".", initialPrompt, repoTrust, hostAuth, debugFlag)
		},
		Version: buildinfo.FormatVersion(),
	}

	rootCmd.AddCommand(newCmd())
	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(serveCmd())
	rootCmd.AddCommand(tokenCmd())
	rootCmd.AddCommand(stateCmd())
	rootCmd.AddCommand(auditCmd())
	rootCmd.AddCommand(usageCmd())
	rootCmd.AddCommand(quotaCmd())
	rootCmd.AddCommand(userCmd())
	rootCmd.AddCommand(previewCmd())
	rootCmd.AddCommand(setupCmd())
	rootCmd.AddCommand(doctorCmd())
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable routing debug trace logging to ~/.config/makewand/trace.jsonl")
	rootCmd.PersistentFlags().StringVar(&repoTrustFlag, "repo-trust", "trusted", "repository trust level: trusted (default) or untrusted (only direct API providers, fail closed)")
	rootCmd.PersistentFlags().StringVar(&rootApprovalFlag, "approval", "", "approval mode for this run: manual, safe, or autopilot (default: configured value; persist with `makewand setup --approval ...`; autopilot still asks before applying until Strength-2 verification exists)")
	rootCmd.Flags().StringVar(&rootModeFlag, "mode", "", "usage mode: fast, balanced, power")
	rootCmd.Flags().StringVar(&rootModeFlag, "tier", "", "alias for --mode: fast, balanced (standard), power (deep)")
	rootCmd.Flags().BoolVar(&rootPrintFlag, "print", false, "run one prompt and print the result (non-interactive)")
	rootCmd.Flags().DurationVar(&rootTimeoutFlag, "timeout", 0, "timeout for --print (default: auto per mode)")

	return rootCmd
}

func newCmd() *cobra.Command {
	var modeFlag string

	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new project with guided wizard",
		Long:  "Start the interactive wizard to create a new project from templates or your own description.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()

			if !hasUsableBackend(cfg) {
				fmt.Println("Welcome to makewand!")
				fmt.Println()
				fmt.Println("No AI models or remote backend found. Install a CLI tool, set an API key, or configure a remote makewand server:")
				fmt.Println()
				fmt.Println("  Option 1: Install Claude Code, Gemini CLI, or Codex CLI (subscription)")
				fmt.Println("  Option 2: Explicitly enable paid API use and set API keys:")
				fmt.Println("    export MAKEWAND_API_POLICY=allow_paid")
				fmt.Println("    export ANTHROPIC_API_KEY=sk-ant-...")
				fmt.Println("    export GEMINI_API_KEY=AI...")
				fmt.Println("  Option 3: Use a remote backend:")
				fmt.Println("    export MAKEWAND_REMOTE_URL=http://your-main-machine:8080")
				fmt.Println("    export MAKEWAND_REMOTE_TOKEN=...")
				fmt.Println()
				fmt.Println("Run 'makewand setup' to check your configuration.")
				return nil
			}

			if modeFlag != "" {
				m, ok := model.ParseUsageMode(modeFlag)
				if !ok {
					return fmt.Errorf("invalid mode %q: must be fast, balanced, or power", modeFlag)
				}
				cfg.UsageMode = m.String()
			}

			repoTrust, trustErr := resolveRepoTrust(repoTrustFlag)
			if trustErr != nil {
				return trustErr
			}

			return tui.Run(tui.ModeNew, cfg, "", repoTrust, resolveUnsafeHostExecAuth(cfg), debugFlag)
		},
	}

	cmd.Flags().StringVar(&modeFlag, "mode", "", "usage mode: fast, balanced, power")
	cmd.Flags().StringVar(&modeFlag, "tier", "", "alias for --mode: fast, balanced (standard), power (deep)")
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

			if !hasUsableBackend(cfg) {
				fmt.Println("No AI models or remote backend configured. Run 'makewand setup' or set MAKEWAND_REMOTE_URL/MAKEWAND_REMOTE_TOKEN.")
				return nil
			}

			if modeFlag != "" {
				m, ok := model.ParseUsageMode(modeFlag)
				if !ok {
					return fmt.Errorf("invalid mode %q: must be fast, balanced, or power", modeFlag)
				}
				cfg.UsageMode = m.String()
			}

			repoTrust, trustErr := resolveRepoTrust(repoTrustFlag)
			if trustErr != nil {
				return trustErr
			}

			projectPath := "."
			if len(args) > 0 {
				projectPath = args[0]
			}

			return tui.Run(tui.ModeChat, cfg, projectPath, repoTrust, resolveUnsafeHostExecAuth(cfg), debugFlag)
		},
	}

	cmd.Flags().StringVar(&modeFlag, "mode", "", "usage mode: fast, balanced, power")
	cmd.Flags().StringVar(&modeFlag, "tier", "", "alias for --mode: fast, balanced (standard), power (deep)")
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
			// Preview needs the config only for the unsafe host-exec
			// acknowledgment state (and its language). Load it ONLY when the
			// opt-in is requested, so a preview run without the env variable
			// keeps its pre-existing behavior of never touching the config.
			if unsafeHostExecRequested() {
				previewCfg, cfgErr := config.LoadWithOptions(config.LoadOptions{SkipCLIDetection: true})
				if cfgErr != nil {
					diag.Stderr().WarnErr("could not load config", cfgErr)
				}
				proj.SetUnsafeHostExecAuthorization(resolveUnsafeHostExecAuth(previewCfg))
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
	var modeFlag string

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Inspect AI providers and save routing preferences",
		Long: `Inspect auto-detected subscription CLIs, API key status, and provider access.
The command keeps a valid configured mode and migrates unset or legacy modes to
balanced. Use --mode to save a different preference without starting the chat UI.
Use the global --approval flag to persist an approval mode (manual/safe/autopilot):
setup saves its configuration, so 'makewand setup --approval safe' makes the
override permanent while '--approval' on other commands applies to that run only.
Note: autopilot only auto-applies candidates verified at Strength 2, which local
checks cannot reach yet (maximum Strength 1), so it currently asks before writing.
When MAKEWAND_UNSAFE_HOST_EXEC=1 is set, setup also offers the one-time host
execution acknowledgment required before that opt-in takes effect.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigWithWarning()
			if err := applySetupUsageMode(cfg, modeFlag); err != nil {
				return err
			}
			// Offer the one-time unsafe host-exec acknowledgment here: setup is
			// the documented interactive path for completing it. The resolver
			// no-ops when MAKEWAND_UNSAFE_HOST_EXEC is unset. It records the
			// acknowledgment on cfg too, so the Save below persists it as well.
			_ = resolveUnsafeHostExecAuth(cfg)

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
			fmt.Printf("API policy: %s (keys alone do not enable paid API use)\n", cfg.EffectiveAPIPolicy())
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
			printRemoteBackendStatus()

			fmt.Printf("  Language: %s\n", cfg.Language)
			fmt.Printf("  Default model: %s\n", cfg.DefaultModel)
			fmt.Printf("  Usage mode: %s\n", cfg.UsageMode)
			fmt.Printf("  Approval mode: %s\n", config.NormalizeApprovalMode(cfg.ApprovalMode))
			if cfg.UnsafeHostExecAckValid() {
				fmt.Printf("  Unsafe host exec: acknowledged v%d at %s (audit: %s)\n", cfg.UnsafeHostExecAckVersion, cfg.UnsafeHostExecAckAt, unsafeHostExecAuditPathDisplay())
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
				fmt.Println("  export MAKEWAND_API_POLICY=allow_paid")
				fmt.Println("  export ANTHROPIC_API_KEY=sk-ant-...")
				fmt.Println("  export GEMINI_API_KEY=AI...")
				fmt.Println("  export OPENAI_API_KEY=sk-...")
				fmt.Println()
				fmt.Println("Or use a remote backend:")
				fmt.Println("  export MAKEWAND_REMOTE_URL=http://your-main-machine:8080")
				fmt.Println("  export MAKEWAND_REMOTE_TOKEN=...")
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
				return fmt.Errorf("save setup configuration: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&modeFlag, "mode", "", "routing mode to save: fast, balanced, or power (default: keep configured mode; balanced when unset)")
	cmd.Flags().StringVar(&modeFlag, "tier", "", "alias for --mode: fast, balanced (standard), power (deep)")
	return cmd
}

func applySetupUsageMode(cfg *config.Config, requested string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		cfg.UsageMode = config.NormalizeUsageMode(cfg.UsageMode)
		return nil
	}

	mode, ok := model.ParseUsageMode(requested)
	if !ok {
		return fmt.Errorf("invalid mode %q: must be fast, balanced, or power", requested)
	}
	cfg.UsageMode = mode.String()
	return nil
}

func accessDisplay(configured, defaultValue string) string {
	if configured != "" {
		return configured
	}
	return defaultValue + " (default)"
}

func loadConfigWithWarning() *config.Config {
	cfg, err := config.LoadWithOptions(configLoadOptions())
	if err != nil {
		diag.Stderr().WarnErr("could not load config", err)
	}
	// Apply the --approval runtime override AFTER load so the flag wins for this
	// process. Commands other than setup never save the config, so the override
	// stays runtime-only; setup saving it is the documented persistence path.
	if resolvedApprovalOverride != "" {
		cfg.ApprovalMode = resolvedApprovalOverride
	}
	return cfg
}

// configLoadOptions derives config.LoadOptions from the resolved repository
// trust. In untrusted mode it skips subscription-CLI detection so config load
// never execs a local CLI (`claude/gemini/codex/agy --version`) inside an
// untrusted working directory; only direct API providers are used there. The
// trusted default keeps CLI detection on, unchanged.
func configLoadOptions() config.LoadOptions {
	return config.LoadOptions{
		SkipCLIDetection: resolvedRepoTrust == model.RepoTrustUntrusted,
	}
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

// readStdinPrompt reads a prompt from piped stdin.
// It only reads when stdin is a pipe or regular file (not a device or socket).
// Reads up to 64KB to prevent unbounded memory usage.
func readStdinPrompt() (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	// Only read if stdin is a pipe or regular file (i.e., has data).
	if info.Mode()&(os.ModeNamedPipe|os.ModeCharDevice) == 0 && !info.Mode().IsRegular() {
		return "", fmt.Errorf("stdin is not a pipe or file")
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return "", fmt.Errorf("stdin is a terminal")
	}
	const maxBytes = 64 * 1024
	reader := bufio.NewReader(io.LimitReader(os.Stdin, maxBytes))
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func runSinglePrompt(cfg *config.Config, prompt string, timeout time.Duration, repoTrust model.RepoTrust, hostAuth engine.UnsafeHostExecAuthorization, debug bool) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt is empty")
	}

	// Honor the configured language in headless mode, matching how the TUI's
	// NewApp does it. Without this a zh-configured user gets English output,
	// including the untrusted-mode refusal message.
	i18n.SetLanguage(cfg.Language)

	// Construct the router WITH the repository trust level so untrusted mode is
	// known before any background work (the async quota refresh, which can exec a
	// local CLI). This fails closed end-to-end instead of relying on a post-hoc
	// SetRepoTrust that lands after the refresh goroutine has started.
	router, err := model.NewRouterWithTrust(cfg, repoTrust)
	if err != nil {
		return fmt.Errorf("initialize model router: %w", err)
	}
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
	project := openHeadlessProject(".")
	project.SetUnsafeHostExecAuthorization(hostAuth)
	systemPrompt := buildHeadlessSystemPrompt(project, task, router.Mode(), prompt, router)

	// Auto-select timeout based on mode when user didn't set --timeout explicitly.
	if timeout <= 0 {
		timeout = headlessTimeoutForMode(router.Mode())
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var (
		content string
		usage   model.Usage
		route   model.RouteResult
	)
	switch {
	case shouldUseHeadlessCandidateSelection(cfg, task, project):
		selection := engine.RunCandidateSelection(
			ctx,
			router,
			project,
			promptTaskToBuildPhase(task),
			messages,
			systemPrompt,
			nil,
		)
		content = selection.Content
		usage = selection.Usage
		route.Actual = selection.Provider
		// Surface delete-only warnings even when the candidate produced no writable
		// content: deletions are never applied automatically and must not be dropped.
		if notice := headlessDeletionNotice(selection.DeletedFiles); notice != "" {
			fmt.Fprintln(os.Stderr, notice)
		}
		// Likewise for the other parts of the candidate that are not in the
		// printed change set, and for how weakly it was verified.
		for _, notice := range headlessSelectionNotices(selection) {
			fmt.Fprintln(os.Stderr, notice)
		}
		if strings.TrimSpace(selection.Content) == "" {
			// Propagate the fail-closed sentinel the engine set when untrusted-repo
			// mode had no untrusted-repo-safe provider, so the mapping below presents
			// the actionable untrusted-mode message instead of the generic one.
			if errors.Is(selection.Err, model.ErrNoUntrustedSafeProvider) {
				err = selection.Err
			} else {
				err = fmt.Errorf("no candidate provider produced a response")
			}
		}
	case router.ModeSet() && router.Mode() == model.ModePower:
		content, usage, route, err = router.ChatBest(ctx, promptTaskToBuildPhase(task), messages, systemPrompt)
	default:
		content, usage, route, err = router.Chat(ctx, task, messages, systemPrompt)
	}
	if dirErr == nil {
		_ = router.SaveStats(configDir)
	}
	if err != nil {
		// Untrusted-repo mode fails closed when no direct-API provider is
		// available. Surface a clear, actionable message (Cobra prints the
		// returned error to stderr) instead of the terse sentinel error, and
		// keep a non-zero exit so headless/CI callers see the refusal.
		if errors.Is(err, model.ErrNoUntrustedSafeProvider) {
			return errors.New(i18n.Msg().RepoTrustNoSafeProvider)
		}
		provider := strings.TrimSpace(usage.Provider)
		if provider == "" {
			provider = strings.TrimSpace(route.Actual)
		}
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.Cost > 0 {
			fmt.Fprintf(os.Stderr, "[makewand] failed request usage: provider=%s input_tokens=%d output_tokens=%d cost_usd=%.6f\n", provider, usage.InputTokens, usage.OutputTokens, usage.Cost)
		}
		return err
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
		if headlessRanHostCLI(router, route, provider) {
			wd, wdErr := os.Getwd()
			if wdErr != nil || wd == "" {
				wd = "."
			}
			fmt.Fprintf(os.Stderr, "[makewand] %s\n", fmt.Sprintf(i18n.Msg().HostCLIExecNotice, provider, wd))
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

// headlessDeletionNotice formats the delete-only warning printed to stderr in
// headless mode. Deletions are never applied automatically, so they must be
// surfaced even when the candidate produced no writable content. Returns "" when
// there are no deletions.
func headlessDeletionNotice(deleted []string) string {
	if len(deleted) == 0 {
		return ""
	}
	return fmt.Sprintf("[makewand] %s",
		fmt.Sprintf(i18n.Msg().AutomationCandidateDeletions, strings.Join(deleted, ", ")))
}

// headlessSelectionNotices formats the stderr notices for a candidate
// selection besides deletions: discarded test-file edits (the printed content
// is the verified set, which keeps the baseline tests), oversized files that
// were not included, and how the printed candidate was (not) verified.
func headlessSelectionNotices(selection engine.CandidateSelection) []string {
	msg := i18n.Msg()
	var notices []string
	if len(selection.RestoredTests) > 0 {
		notices = append(notices, "[makewand] "+fmt.Sprintf(msg.AutomationCandidateRestoredTests, strings.Join(selection.RestoredTests, ", ")))
	}
	if len(selection.LargeFiles) > 0 {
		notices = append(notices, "[makewand] "+fmt.Sprintf(msg.AutomationCandidateLargeFiles, strings.Join(selection.LargeFiles, ", ")))
	}
	if strings.TrimSpace(selection.Content) != "" && !selection.Verified {
		switch {
		case selection.NotVerifiedReason != "":
			notices = append(notices, "[makewand] "+fmt.Sprintf(msg.AutomationCandidateIsolationUnavailable, selection.NotVerifiedReason))
		case selection.NoTestsExecuted:
			notices = append(notices, "[makewand] "+msg.AutomationCandidateNoTests)
		case selection.Strength > 0:
			notices = append(notices, "[makewand] "+msg.AutomationCandidateWeakVerification)
		}
	}
	return notices
}

func shouldUseHeadlessCandidateSelection(cfg *config.Config, task model.TaskType, project *engine.Project) bool {
	if project == nil || cfg == nil {
		return false
	}
	if config.NormalizeApprovalMode(cfg.ApprovalMode) != config.ApprovalModeAuto {
		return false
	}
	switch task {
	case model.TaskCode, model.TaskFix:
		return true
	default:
		return false
	}
}

const headlessProjectScanEntryLimit = 512

func openHeadlessProject(path string) *engine.Project {
	proj, err := engine.OpenProjectLimited(path, headlessProjectScanEntryLimit)
	if err != nil {
		return nil
	}
	return proj
}

func newHeadlessTraceSink() (*diag.JSONLTraceSink, string, error) {
	var candidates []string
	if dir, err := config.ConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "trace.jsonl"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "makewand-trace.jsonl"))
	return diag.OpenFirstJSONLTraceSink(candidates)
}

// headlessTimeoutForMode returns a sensible default --print timeout per mode.
// Fast mode gets a tight budget so total wall time stays short even with fallbacks.
func headlessTimeoutForMode(mode model.UsageMode) time.Duration {
	switch mode {
	case model.ModeFast:
		return 90 * time.Second
	case model.ModeBalanced:
		return 3 * time.Minute
	default: // power — ensemble needs more room
		return 5 * time.Minute
	}
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

// headlessRanHostCLI reports whether the resolved provider executed a local CLI
// on the host (as opposed to a direct HTTP API provider). Generation via a local
// CLI is not sandboxed, so callers surface a one-time notice. The route's Provider
// is nil on the synthesized candidate-selection path, so fall back to the
// subscription flag, which marks the host-executing CLI providers.
func headlessRanHostCLI(r *model.Router, route model.RouteResult, provider string) bool {
	if _, ok := route.Provider.(*model.CLIProvider); ok {
		return true
	}
	return r != nil && r.IsSubscription(provider)
}

func buildHeadlessSystemPrompt(project *engine.Project, task model.TaskType, mode model.UsageMode, prompt string, r *model.Router) string {
	base := tui.BuildSystemPrompt(project, task, mode, r)
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
