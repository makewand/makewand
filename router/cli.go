package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
)

// Prompt delivery modes for custom command providers.
// These match PromptModeStdin and PromptModeArg
// but are inlined here to keep the model package free of config dependencies.
const (
	PromptModeStdin = "stdin" // pass prompt via stdin
	PromptModeArg   = "arg"   // pass prompt as trailing CLI argument
)

// CLIProvider wraps a subscription CLI tool (claude, gemini, codex) as a Provider.
// It calls the CLI in non-interactive/print mode, which uses the user's subscription.
type CLIProvider struct {
	name           string // "claude-cli", "gemini-cli", "codex-cli"
	binPath        string // absolute path to the binary
	provider       string // display name: "claude", "gemini", "codex"
	buildCmd       func(ctx context.Context, prompt string) *exec.Cmd
	buildStreamCmd func(ctx context.Context, prompt string) *exec.Cmd
	checkCmd       func(ctx context.Context) *exec.Cmd

	// jsonOutput indicates the CLI is invoked with a JSON output flag.
	// When true, chatAttempt tries parseJSONResponse before falling back to text.
	jsonOutput        bool
	parseJSONResponse func(raw []byte) (content string, usage *Usage, err error)
	parseStreamLine   func(line string) ([]StreamChunk, error)

	// systemFlag, if non-empty, is the CLI flag for passing system prompts
	// separately (e.g. "--append-system-prompt" for Claude CLI).
	// When set, Chat passes system via this flag and only user messages in -p.
	systemFlag string

	mu            sync.Mutex
	cachedAvail   bool
	cachedAvailAt time.Time
}

const cliAvailCacheTTL = 60 * time.Second
const cliAvailProbeTimeout = 4 * time.Second
const cliRequestTimeout = 5 * time.Minute
const cliStreamTimeout = 10 * time.Minute
const cliMaxAttempts = 2
const cliRetryBaseDelay = 300 * time.Millisecond

// TransientCLIError wraps a CLI execution error that is safe to retry.
type TransientCLIError struct {
	Err *ProviderError
}

func (e *TransientCLIError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *TransientCLIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

var transientCLIErrorHints = []string{
	"stream closed unexpectedly",
	"premature close",
	"transport error",
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"tls handshake timeout",
}

// --- Claude CLI ---

// NewClaudeCLI creates a provider that uses `claude -p` (Claude Code CLI).
func NewClaudeCLI(binPath string) *CLIProvider {
	p := &CLIProvider{
		name:              "claude-cli",
		binPath:           binPath,
		provider:          "claude",
		jsonOutput:        true,
		parseJSONResponse: parseClaudeCLIJSON,
		systemFlag:        "--append-system-prompt",
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		args := []string{"-p", prompt, "--output-format", "json"}
		// Per-call model selection: use --model to select tier-specific models
		// (e.g. haiku for fast, sonnet for balanced, opus for power).
		if modelID, ok := ModelFromContext(ctx); ok && !strings.HasSuffix(modelID, "-cli") {
			args = append(args, "--model", modelID)
		}
		// System prompt via dedicated flag (proper system role, not user message).
		if sys, ok := SystemFromContext(ctx); ok {
			args = append(args, "--append-system-prompt", sys)
		}
		cmd := exec.CommandContext(ctx, binPath, args...)
		// Unset CLAUDECODE to allow nested invocation from within Claude Code
		cmd.Env = filterEnv(cmd.Environ(), "CLAUDECODE")
		return cmd
	}
	p.buildStreamCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		args := []string{"-p", prompt, "--output-format", "text"}
		if modelID, ok := ModelFromContext(ctx); ok && !strings.HasSuffix(modelID, "-cli") {
			args = append(args, "--model", modelID)
		}
		if sys, ok := SystemFromContext(ctx); ok {
			args = append(args, "--append-system-prompt", sys)
		}
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Env = filterEnv(cmd.Environ(), "CLAUDECODE")
		return cmd
	}
	p.checkCmd = func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binPath, "--version")
		cmd.Env = filterEnv(cmd.Environ(), "CLAUDECODE")
		return cmd
	}
	return p
}

// --- Gemini CLI ---

// NewGeminiCLI creates a provider that uses `gemini -p` (Gemini CLI).
func NewGeminiCLI(binPath string) *CLIProvider {
	p := &CLIProvider{
		name:              "gemini-cli",
		binPath:           binPath,
		provider:          "gemini",
		jsonOutput:        true,
		parseJSONResponse: parseGeminiCLIJSON,
		parseStreamLine:   parseGeminiCLIStreamJSONLine,
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		args := []string{"-p", prompt, "--sandbox", "false", "--output-format", "json"}
		// Per-call model selection: use --model to select tier-specific models
		// (e.g. flash for fast/balanced, pro for power).
		if modelID, ok := ModelFromContext(ctx); ok && !strings.HasSuffix(modelID, "-cli") {
			args = append(args, "--model", modelID)
		}
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Env = applyGeminiProxyPolicy(cmd.Environ())
		return cmd
	}
	p.buildStreamCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		args := []string{"-p", prompt, "--sandbox", "false", "--output-format", "stream-json"}
		if modelID, ok := ModelFromContext(ctx); ok && !strings.HasSuffix(modelID, "-cli") {
			args = append(args, "--model", modelID)
		}
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Env = applyGeminiProxyPolicy(cmd.Environ())
		return cmd
	}
	p.checkCmd = func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binPath, "--version")
		cmd.Env = applyGeminiProxyPolicy(cmd.Environ())
		return cmd
	}
	return p
}

// --- Agy CLI (Antigravity) ---

// NewAgyCLI creates a provider that uses `agy --print` (Antigravity CLI).
//
// As of June 2026 Google retired the individual-account OAuth path for the old
// `gemini` CLI; personal Gemini subscriptions now flow exclusively through the
// Antigravity CLI (`agy`). This provider is registered under the logical name
// "gemini" so it slots into the existing gemini strategy-table entries, but its
// transport is agy — the `gemini -p` metered API path is intentionally not used.
//
// Model selection is deliberately left to agy's own configured default (set via
// agy's `/model` UI) rather than forced with --model: agy's model list mixes
// Gemini, Claude, and GPT models drawn from *different* quota groups, and forcing
// a makewand tier model id (e.g. "gemini-2.5-flash", which agy does not know)
// would either error or silently cross pools. Override with MAKEWAND_AGY_MODEL.
func NewAgyCLI(binPath string) *CLIProvider {
	p := &CLIProvider{
		name:     "agy-cli",
		binPath:  binPath,
		provider: "gemini",
	}
	buildArgs := func(prompt string) []string {
		// agy's --print is a string flag whose next argument is the prompt. Keep
		// every global option before it; otherwise `--print --sandbox <prompt>`
		// makes agy treat the literal "--sandbox" as the prompt and ignore the
		// actual user input.
		args := []string{"--sandbox"}
		if m := strings.TrimSpace(os.Getenv("MAKEWAND_AGY_MODEL")); m != "" {
			args = append(args, "--model", m)
		}
		args = append(args, "--print", prompt)
		return args
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		//nolint:gosec // G204: binPath is a resolved trusted CLI; args are internally built, not user-tainted (same pattern as the other providers).
		return exec.CommandContext(ctx, binPath, buildArgs(prompt)...)
	}
	p.buildStreamCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		//nolint:gosec // G204: see buildCmd above.
		return exec.CommandContext(ctx, binPath, buildArgs(prompt)...)
	}
	p.checkCmd = func(ctx context.Context) *exec.Cmd {
		// `agy models` exits 0 only when signed in — doubles as an auth probe.
		return exec.CommandContext(ctx, binPath, "models")
	}
	return p
}

// --- Codex CLI ---

// NewCodexCLI creates a provider that uses `codex exec` (Codex CLI).
// Task-aware: uses `codex review --uncommitted` for local review tasks,
// `codex exec --json` for code/analysis tasks (provides structured usage data).
// Remote-origin requests (see ContextWithRemoteOrigin) never use the review
// subcommand: it ignores the prompt and reviews the serving host's working
// tree, so they always run the caller's prompt through `codex exec`.
func NewCodexCLI(binPath string) *CLIProvider {
	p := &CLIProvider{
		name:              "codex-cli",
		binPath:           binPath,
		provider:          "codex",
		jsonOutput:        true,
		parseJSONResponse: parseCodexCLIJSONL,
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		// Use dedicated review subcommand for local review tasks.
		if codexUsesReviewSubcommand(ctx) {
			args := []string{"review", "--uncommitted"}
			return exec.CommandContext(ctx, binPath, args...)
		}
		// Default: exec with JSON output for structured usage data.
		args := []string{
			"exec",
			"--sandbox", codexSandboxMode(ctx),
			"--ephemeral",
			"--json",
			"--skip-git-repo-check",
			prompt,
		}
		return exec.CommandContext(ctx, binPath, args...)
	}
	p.buildStreamCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		if codexUsesReviewSubcommand(ctx) {
			args := []string{"review", "--uncommitted"}
			return exec.CommandContext(ctx, binPath, args...)
		}
		args := []string{
			"exec",
			"--sandbox", codexSandboxMode(ctx),
			"--ephemeral",
			"--skip-git-repo-check",
			prompt,
		}
		return exec.CommandContext(ctx, binPath, args...)
	}
	p.checkCmd = func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, binPath, "--version")
	}
	return p
}

// NewCommandCLI creates a provider backed by an arbitrary CLI command.
// Prompt delivery modes:
//   - stdin: prompt is written to process stdin
//   - arg: prompt is appended as the final argument
//   - legacy: if any arg contains "{{prompt}}" replace it; otherwise append the
//     prompt as the final argument (kept for backward compatibility)
func NewCommandCLI(providerName, command string, args []string, promptMode string) *CLIProvider {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	command = strings.TrimSpace(command)
	templateArgs := append([]string(nil), args...)
	promptMode = strings.ToLower(strings.TrimSpace(promptMode))

	p := &CLIProvider{
		name:     providerName + "-custom-cli",
		binPath:  command,
		provider: providerName,
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		if promptMode == PromptModeStdin {
			cmd := exec.CommandContext(ctx, command, templateArgs...)
			cmd.Stdin = strings.NewReader(prompt)
			return cmd
		}

		finalArgs := make([]string, 0, len(templateArgs)+1)
		if promptMode == PromptModeArg {
			finalArgs = append(finalArgs, templateArgs...)
			finalArgs = append(finalArgs, prompt)
			return exec.CommandContext(ctx, command, finalArgs...)
		}

		finalArgs = make([]string, 0, len(templateArgs)+1)
		replaced := false
		for _, a := range templateArgs {
			if strings.Contains(a, "{{prompt}}") {
				a = strings.ReplaceAll(a, "{{prompt}}", prompt)
				replaced = true
			}
			finalArgs = append(finalArgs, a)
		}
		if !replaced {
			finalArgs = append(finalArgs, prompt)
		}
		return exec.CommandContext(ctx, command, finalArgs...)
	}
	return p
}

// Provider interface implementation

func (c *CLIProvider) Name() string { return c.provider }

// SafeForUntrustedRepo reports that a CLI provider is NOT safe against an
// untrusted repo: it execs a local subscription CLI (claude/codex/gemini-cli/agy/
// custom command) on the host with the user's env and credentials and cwd = the
// repo, so repo content (instruction files, .mcp.json, prompt injection) can steer
// that host-privileged agent into a confused-deputy action.
func (c *CLIProvider) SafeForUntrustedRepo() bool { return false }

// ReportedModelID distinguishes a requested tier model from what the CLI can
// actually guarantee. Claude and Gemini accept explicit model flags. Codex and
// Agy currently choose their own configured/default model unless Agy has an
// explicit Makewand override, so exposing a premium model ID for those calls
// would be misleading.
func (c *CLIProvider) ReportedModelID(requestedModelID string) string {
	if c == nil {
		return requestedModelID
	}
	switch c.name {
	case "agy-cli":
		if modelID := strings.TrimSpace(os.Getenv("MAKEWAND_AGY_MODEL")); modelID != "" {
			return modelID
		}
		return "agy-provider-managed"
	case "codex-cli":
		return "codex-provider-managed"
	default:
		return requestedModelID
	}
}

func (c *CLIProvider) IsAvailable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Since(c.cachedAvailAt) < cliAvailCacheTTL {
		return c.cachedAvail
	}

	c.cachedAvail = c.healthCheck()
	c.cachedAvailAt = time.Now()
	return c.cachedAvail
}

func (c *CLIProvider) healthCheck() bool {
	if _, err := exec.LookPath(c.binPath); err != nil {
		return false
	}

	// Custom command providers may not have a safe version/health command;
	// for them, PATH lookup is the availability signal.
	if c.checkCmd == nil {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), cliAvailProbeTimeout)
	defer cancel()

	cmd := c.checkCmd(ctx)
	if cmd == nil {
		return false
	}
	setCLIProcessGroup(cmd)
	capture := processjob.NewCapture(processjob.MaxOutputBytes, func() { killCLIProcess(cmd) })
	cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
	cleanupProcess, err := processjob.Start(cmd)
	if err != nil {
		return false
	}
	defer cleanupProcess()
	defer killCLIProcess(cmd)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	return c.waitHealthProbe(ctx, done, capture, func() { killCLIProcess(cmd) })
}

// waitHealthProbe joins the supervised probe before classifying its outcome.
// Wait and the deadline can become ready together; availability must not depend
// on which select arm wins that race.
func (c *CLIProvider) waitHealthProbe(ctx context.Context, done <-chan error, capture *processjob.Capture, stop func()) bool {
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		stop()
		err = <-done
	}
	if capture.Exceeded() {
		return false
	}
	if ctx.Err() != nil {
		// Some subscription CLIs stall on version/auth probes. Their deadline is
		// unknown availability, while custom probes and cancellation fail closed.
		return errors.Is(ctx.Err(), context.DeadlineExceeded) && c.softPassProbeTimeout()
	}
	return err == nil
}

func (c *CLIProvider) softPassProbeTimeout() bool {
	switch c.provider {
	case "gemini", "claude", "codex", "muse", "grok":
		return true
	default:
		return false
	}
}

func (c *CLIProvider) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	var prompt string
	validationPrompt := buildCLIValidationPrompt(messages)
	if c.systemFlag != "" && system != "" {
		// Pass system prompt via dedicated CLI flag (proper system role).
		prompt = buildCLIPrompt(messages, "")
		ctx = ContextWithSystem(ctx, system)
	} else {
		prompt = buildCLIPrompt(messages, system)
	}

	// Only add CLI-level timeout if the caller hasn't set a tighter deadline.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cliCancel context.CancelFunc
		ctx, cliCancel = context.WithTimeout(ctx, cliRequestTimeout)
		defer cliCancel()
	}

	// The scratch dir is kept out of ctx on purpose: ctx flows into every
	// exec.CommandContext, and deriving it from a filesystem-created value
	// would taint all of them for gosec's G702 analysis.
	scratchDir, cleanupWorkDir, err := prepareRemoteCLIWorkDir(ctx, c.provider)
	if err != nil {
		return "", Usage{}, err
	}
	// Every attempt has been waited for when Chat returns, so the scratch dir
	// is gone before the caller sees the result.
	defer cleanupWorkDir()

	attempts := 0
	for {
		attempts++
		content, jsonUsage, err := c.chatAttempt(ctx, prompt, validationPrompt, scratchDir)
		if err == nil {
			var usage Usage
			if jsonUsage != nil {
				// Use real usage from structured JSON output.
				usage = *jsonUsage
				// Ensure provider fields are set even if the parser omitted them.
				if usage.Provider == "" {
					usage.Provider = c.provider
				}
				if usage.Model == "" {
					usage.Model = c.provider + "-cli"
				}
			} else {
				// Fallback to heuristic estimation (codex, or JSON parse failure).
				usage = Usage{
					InputTokens:  estimateTokens(prompt),
					OutputTokens: estimateTokens(content),
					Cost:         0, // Subscription — no per-token cost
					Model:        c.provider + "-cli",
					Provider:     c.provider,
				}
			}
			return content, usage, nil
		}

		if attempts >= cliMaxAttempts || !isTransientCLIError(err) || ctx.Err() != nil {
			if attempts > 1 {
				return "", Usage{}, fmt.Errorf("%w (after %d attempts)", err, attempts)
			}
			return "", Usage{}, err
		}

		delay := cliRetryDelay(attempts)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if attempts > 1 {
				return "", Usage{}, fmt.Errorf("%w (after %d attempts)", err, attempts)
			}
			return "", Usage{}, err
		case <-timer.C:
		}
	}
}

func (c *CLIProvider) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	ctx, attempt, err := reserveProvider(ctx, c)
	if err != nil {
		return nil, err
	}
	stream, err := c.chatStreamUnaccounted(ctx, messages, system, maxTokens)
	if err != nil {
		return nil, completeProvider(attempt, Usage{}, err)
	}
	return observeDispatchStream(ctx, stream, attempt), nil
}

func (c *CLIProvider) chatStreamUnaccounted(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	var prompt string
	if c.systemFlag != "" && system != "" {
		prompt = buildCLIPrompt(messages, "")
		ctx = ContextWithSystem(ctx, system)
	} else {
		prompt = buildCLIPrompt(messages, system)
	}

	// Only add CLI-level stream timeout if the caller hasn't set a tighter deadline.
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, cancel = context.WithTimeout(ctx, cliStreamTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	scratchDir, cleanupWorkDir, err := prepareRemoteCLIWorkDir(ctx, c.provider)
	if err != nil {
		cancel()
		return nil, err
	}

	buildCmd := c.buildStreamCmd
	if buildCmd == nil {
		buildCmd = c.buildCmd
	}
	cmd := buildCmd(ctx, prompt)
	applyCLIWorkDir(ctx, cmd, scratchDir)
	wrappedCmd, err := wrapCLICommandWithSandbox(ctx, c.provider, cmd)
	if err != nil {
		cancel()
		cleanupWorkDir()
		return nil, err
	}
	cmd = wrappedCmd
	setCLIProcessGroup(cmd)
	// Let exec own the OS pipes so WaitDelay can bound inherited descriptors
	// after leader exit. Waiting only after Scanner sees EOF leaves that bound
	// inactive whenever a descendant keeps stdout open.
	stdoutPipe, stdoutWriter := io.Pipe()
	capture := processjob.NewCapture(processjob.MaxOutputBytes, func() { killCLIProcess(cmd) })
	cmd.Stdout = io.MultiWriter(capture.Stdout(), stdoutWriter)
	cmd.Stderr = capture.Stderr()

	started := time.Now()
	cleanupProcess, err := processjob.Start(cmd)
	if err != nil {
		contextErr := ctx.Err()
		_ = stdoutPipe.Close()
		_ = stdoutWriter.Close()
		cancel()
		cleanupWorkDir()
		if contextErr != nil {
			return nil, c.formatExecutionError(capture.StdoutBytes(), capture.StderrString(), err, errors.Join(contextErr, err), time.Since(started))
		}
		return nil, newProviderError(c.provider, "CLI start", ErrorKindConfig, false, 0, err.Error(), err)
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer cleanupProcess()
		defer killCLIProcess(cmd)
		defer cancel()
		defer close(ch)
		// Safety net; the normal path removes the scratch dir right after
		// cmd.Wait, before any terminal chunk reaches the consumer.
		defer cleanupWorkDir()
		defer func() { _ = stdoutPipe.Close() }()
		start := time.Now()

		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-ctx.Done():
				killCLIProcess(cmd)
				_ = stdoutPipe.CloseWithError(ctx.Err())
			case <-watchDone:
			}
		}()
		waitDone := make(chan error, 1)
		go func() {
			waitErr := cmd.Wait()
			killCLIProcess(cmd)
			_ = stdoutWriter.Close()
			waitDone <- waitErr
		}()

		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var parseFailure error
		var claudeFailureJSON []byte
	streamLoop:
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				killCLIProcess(cmd)
				break streamLoop
			default:
			}
			line := stripANSI(scanner.Text())
			if line == "" {
				continue
			}
			if c.name == "claude-cli" && claudeCLIErrorMessage([]byte(line)) != "" {
				// Never stream the failure envelope's session/account metadata.
				// Wait for the process outcome before classifying the error.
				claudeFailureJSON = []byte(line)
				continue
			}

			if c.parseStreamLine != nil {
				chunks, parseErr := c.parseStreamLine(line)
				if parseErr != nil {
					parseFailure = newProviderError(c.provider, "CLI stream parse", ErrorKindProvider, false, 0, parseErr.Error(), parseErr)
					killCLIProcess(cmd)
					break streamLoop
				}
				for _, chunk := range chunks {
					if chunk.Error != nil {
						parseFailure = chunk.Error
						killCLIProcess(cmd)
						break streamLoop
					}
					// A vendor's logical done event is provisional until the
					// process exits successfully; otherwise a late failure can be
					// recorded as success and hide an unknown dispatch outcome.
					if chunk.Done {
						chunk.Done = false
						if chunk.Content == "" {
							continue
						}
					}
					select {
					case ch <- chunk:
					case <-ctx.Done():
						killCLIProcess(cmd)
						break streamLoop
					}
				}
				continue
			}

			select {
			case ch <- StreamChunk{Content: line + "\n"}:
			case <-ctx.Done():
				killCLIProcess(cmd)
				break streamLoop
			}
		}

		scanErr := scanner.Err()
		if scanErr != nil || parseFailure != nil || ctx.Err() != nil {
			killCLIProcess(cmd)
			// An abandoned parser must release exec's stdout copying goroutine.
			_ = stdoutPipe.Close()
		}
		waitErr := <-waitDone
		cleanupWorkDir()
		duration := time.Since(start)
		if capture.Exceeded() {
			ch <- StreamChunk{Error: &execution.UnknownOutcomeError{Err: processjob.OutputLimitError()}}
			return
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			if shouldSurfaceContextError(ctxErr) {
				ch <- StreamChunk{Error: formatCLIContextError(c.provider, ctxErr, duration)}
			}
			return
		}

		if scanErr != nil {
			ch <- StreamChunk{Error: &execution.UnknownOutcomeError{Err: fmt.Errorf("%s CLI stream could not be fully read: %w", c.provider, scanErr)}}
			return
		}
		if parseFailure != nil {
			ch <- StreamChunk{Error: parseFailure, Done: true}
			return
		}

		if waitErr != nil || claudeFailureJSON != nil {
			stdout := capture.StdoutBytes()
			if claudeFailureJSON != nil {
				stdout = claudeFailureJSON
				if waitErr == nil {
					waitErr = errors.New("CLI reported an error")
				}
			}
			ch <- StreamChunk{Error: c.formatExecutionError(stdout, capture.StderrString(), waitErr, nil, duration)}
			return
		}

		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

// --- JSON Output Parsing ---

// claudeCLIJSON represents the structured JSON output from `claude -p --output-format json`.
type claudeCLIJSON struct {
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	// modelUsage maps model name to per-model stats; we pick the first entry.
	ModelUsage map[string]struct {
		InputTokens  int     `json:"inputTokens"`
		OutputTokens int     `json:"outputTokens"`
		CostUSD      float64 `json:"costUSD"`
	} `json:"modelUsage"`
}

// parseClaudeCLIJSON extracts the response text and real usage from Claude CLI JSON output.
func parseClaudeCLIJSON(raw []byte) (string, *Usage, error) {
	var resp claudeCLIJSON
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", nil, err
	}
	content := strings.TrimSpace(resp.Result)
	if content == "" {
		return "", nil, fmt.Errorf("empty result in claude JSON response")
	}

	usage := &Usage{
		InputTokens:    resp.Usage.InputTokens,
		OutputTokens:   resp.Usage.OutputTokens,
		Cost:           resp.TotalCostUSD,
		Provider:       "claude",
		MeasuredTokens: resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0,
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		value, exists := fields["total_cost_usd"]
		usage.MeasuredCost = exists && string(value) != "null"
	}

	// Extract model name and per-model cost from modelUsage if available.
	for modelName, mu := range resp.ModelUsage {
		usage.Model = modelName
		if mu.InputTokens > 0 {
			usage.InputTokens = mu.InputTokens
		}
		if mu.OutputTokens > 0 {
			usage.OutputTokens = mu.OutputTokens
		}
		if mu.CostUSD > 0 {
			usage.Cost = mu.CostUSD
			usage.MeasuredCost = true
		}
		break // Take the first (usually only) model entry.
	}
	usage.MeasuredTokens = usage.InputTokens > 0 || usage.OutputTokens > 0

	return content, usage, nil
}

// geminiCLIJSON represents the structured JSON output from `gemini -p --output-format json`.
type geminiCLIJSON struct {
	Response string `json:"response"`
	Stats    struct {
		Models map[string]struct {
			Tokens struct {
				Input      int `json:"input"`
				Candidates int `json:"candidates"`
				Total      int `json:"total"`
			} `json:"tokens"`
		} `json:"models"`
	} `json:"stats"`
}

type geminiCLIStreamJSON struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// parseGeminiCLIJSON extracts the response text and real usage from Gemini CLI JSON output.
func parseGeminiCLIJSON(raw []byte) (string, *Usage, error) {
	var resp geminiCLIJSON
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", nil, err
	}
	content := strings.TrimSpace(resp.Response)
	if content == "" {
		return "", nil, fmt.Errorf("empty response in gemini JSON response")
	}

	usage := &Usage{
		Provider: "gemini",
		Cost:     0, // Gemini CLI is free-tier; no per-token cost.
	}

	// Aggregate token counts across all models used in the session.
	var totalInput, totalOutput int
	var modelName string
	for name, m := range resp.Stats.Models {
		totalInput += m.Tokens.Input
		totalOutput += m.Tokens.Candidates
		modelName = name // Keep last model name as representative.
	}
	usage.InputTokens = totalInput
	usage.OutputTokens = totalOutput
	usage.MeasuredTokens = totalInput > 0 || totalOutput > 0
	usage.Model = modelName

	return content, usage, nil
}

func parseGeminiCLIStreamJSONLine(line string) ([]StreamChunk, error) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		// Gemini CLI can emit plain-text prelude lines (for example credential/cache
		// notices) before the NDJSON event stream starts. Ignore those lines.
		return nil, nil
	}

	var event geminiCLIStreamJSON
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return nil, fmt.Errorf("parse gemini stream-json line: %w", err)
	}

	switch event.Type {
	case "message":
		if event.Role == "assistant" && event.Content != "" {
			return []StreamChunk{{Content: event.Content}}, nil
		}
		return nil, nil
	case "result":
		if strings.EqualFold(event.Status, "success") {
			return []StreamChunk{{Done: true}}, nil
		}
		msg := strings.TrimSpace(event.Message)
		if event.Error != nil && strings.TrimSpace(event.Error.Message) != "" {
			msg = strings.TrimSpace(event.Error.Message)
		}
		if msg == "" {
			msg = "gemini stream failed"
		}
		return []StreamChunk{{
			Error: newProviderError("gemini", "CLI stream", ErrorKindProvider, false, 0, msg, nil),
		}}, nil
	default:
		return nil, nil
	}
}

// --- Codex CLI JSONL Parsing ---

// codexJSONLEvent represents a single line in Codex CLI `exec --json` JSONL output.
// Actual format (codex exec --json --skip-git-repo-check):
//
//	{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"..."}}
//	{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"...","aggregated_output":"...","exit_code":0,"status":"completed"}}
//	{"type":"turn.completed","usage":{"input_tokens":25629,"cached_input_tokens":16384,"output_tokens":341}}
type codexJSONLEvent struct {
	Type string `json:"type"`
	Item struct {
		Type             string `json:"type"`
		Text             string `json:"text"`              // agent_message text
		AggregatedOutput string `json:"aggregated_output"` // command_execution output
		Status           string `json:"status"`
	} `json:"item"`
	Usage struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage"`
}

// parseCodexCLIJSONL extracts the response text and real usage from Codex CLI
// `exec --json` JSONL output. It collects text from `item.completed` agent_message
// events and usage from the `turn.completed` event.
func parseCodexCLIJSONL(raw []byte) (string, *Usage, error) {
	lines := bytes.Split(raw, []byte("\n"))

	var textParts []string
	var bestUsage *Usage

	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var event codexJSONLEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip non-JSON lines
		}

		switch event.Type {
		case "item.completed":
			// Collect text from agent_message items (the actual AI response).
			if event.Item.Type == "agent_message" && event.Item.Text != "" {
				textParts = append(textParts, event.Item.Text)
			}
		case "turn.completed":
			// Extract real token usage from the turn summary.
			if event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
				bestUsage = &Usage{
					MeasuredTokens: true,
					InputTokens:    event.Usage.InputTokens,
					OutputTokens:   event.Usage.OutputTokens,
					Cost:           0, // Codex CLI uses subscription; no per-token cost.
					Provider:       "codex",
				}
			}
		}
	}

	if len(textParts) == 0 {
		return "", nil, fmt.Errorf("no agent_message items found in codex JSONL output")
	}

	return strings.TrimSpace(strings.Join(textParts, "\n")), bestUsage, nil
}

// --- Helpers ---

func shouldSurfaceContextError(ctxErr error) bool {
	return errors.Is(ctxErr, context.DeadlineExceeded)
}

// applyCLIWorkDir sets the directory a CLI command runs in. An explicit
// caller-chosen WorkDir (ContextWithWorkDir) wins. Otherwise scratchDir, the
// per-call directory from prepareRemoteCLIWorkDir, is used when non-empty.
// Local calls without a WorkDir keep inheriting the process cwd.
func applyCLIWorkDir(ctx context.Context, cmd *exec.Cmd, scratchDir string) {
	if cmd == nil {
		return
	}
	if dir, ok := WorkDirFromContext(ctx); ok {
		cmd.Dir = dir
		return
	}
	if scratchDir != "" {
		// Go resolves a relative Cmd.Path against Cmd.Dir. A custom command
		// such as "./bin/llm" is accepted by config and probed by IsAvailable
		// relative to the process cwd, so pin it there before moving the
		// process into the empty scratch dir; otherwise every remote call to
		// an "available" provider fails with "no such file or directory".
		pinRelativeCommandPath(cmd)
		cmd.Dir = scratchDir
	}
}

// pinRelativeCommandPath makes a relative command path that contains a path
// separator (e.g. "./bin/llm", "tools/llm") absolute against the current
// process working directory. Bare names were already resolved through PATH by
// exec.Command and absolute paths are left untouched.
func pinRelativeCommandPath(cmd *exec.Cmd) {
	if cmd == nil || cmd.Path == "" || filepath.IsAbs(cmd.Path) {
		return
	}
	if !strings.ContainsRune(cmd.Path, filepath.Separator) {
		return
	}
	abs, err := filepath.Abs(cmd.Path)
	if err != nil {
		return
	}
	cmd.Path = abs
}

// prepareRemoteCLIWorkDir isolates a remote-origin CLI call that has no
// explicit WorkDir: it returns a freshly created, empty, private (0700)
// temporary directory for the command to run in instead of inheriting the
// server process's cwd, so a remote caller can never steer the agent at the
// serving host's working tree through relative paths or cwd-discovered
// instruction files. The returned cleanup removes the directory and is
// idempotent. Local calls and calls with an explicit WorkDir get an empty dir
// and a no-op cleanup.
//
// The directory is returned as a plain value rather than stored in ctx: ctx is
// passed to every buildCmd/exec.CommandContext, and it must stay free of
// filesystem-derived values (gosec G702 taint analysis).
func prepareRemoteCLIWorkDir(ctx context.Context, provider string) (string, func(), error) {
	noop := func() {}
	if !RemoteOriginFromContext(ctx) {
		return "", noop, nil
	}
	if _, ok := WorkDirFromContext(ctx); ok {
		return "", noop, nil
	}
	dir, err := os.MkdirTemp("", "makewand-remote-cli-*")
	if err != nil {
		return "", noop, newProviderError(provider, "CLI scratch dir", ErrorKindConfig, false, 0, err.Error(), err)
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() { _ = os.RemoveAll(dir) })
	}
	return dir, cleanup, nil
}

// codexUsesReviewSubcommand reports whether a Codex call should run
// `codex review --uncommitted`. Only local review tasks do: the subcommand
// ignores the prompt and reviews the uncommitted changes of the current
// directory, which for a remote caller would be the serving host's state.
func codexUsesReviewSubcommand(ctx context.Context) bool {
	if RemoteOriginFromContext(ctx) {
		return false
	}
	task, ok := TaskFromContext(ctx)
	return ok && task == TaskReview
}

func codexSandboxMode(ctx context.Context) string {
	if task, ok := TaskFromContext(ctx); ok && task == TaskReview {
		return "read-only"
	}
	// Remote callers never get a writable Codex sandbox, even when an embedder
	// supplied an explicit WorkDir.
	if RemoteOriginFromContext(ctx) {
		return "read-only"
	}
	if _, ok := WorkDirFromContext(ctx); ok {
		return "workspace-write"
	}
	return "read-only"
}

func (c *CLIProvider) chatAttempt(ctx context.Context, prompt, validationPrompt, scratchDir string) (string, *Usage, error) {
	ctx, attempt, err := reserveProvider(ctx, c)
	if err != nil {
		return "", nil, err
	}
	content, usage, err := c.chatReservedAttempt(ctx, prompt, validationPrompt, scratchDir)
	var observed Usage
	if usage != nil {
		observed = *usage
	}
	return content, usage, completeProvider(attempt, observed, err)
}

func (c *CLIProvider) chatReservedAttempt(ctx context.Context, prompt, validationPrompt, scratchDir string) (string, *Usage, error) {
	cmd := c.buildCmd(ctx, prompt)
	applyCLIWorkDir(ctx, cmd, scratchDir)
	wrappedCmd, err := wrapCLICommandWithSandbox(ctx, c.provider, cmd)
	if err != nil {
		return "", nil, err
	}
	cmd = wrappedCmd
	setCLIProcessGroup(cmd)

	capture := processjob.NewCapture(processjob.MaxOutputBytes, func() { killCLIProcess(cmd) })
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()

	start := time.Now()
	cleanupProcess, err := processjob.Start(cmd)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", nil, c.formatExecutionError(capture.StdoutBytes(), capture.StderrString(), err, errors.Join(contextErr, err), time.Since(start))
		}
		return "", nil, newProviderError(c.provider, "CLI start", ErrorKindConfig, false, 0, err.Error(), err)
	}
	defer cleanupProcess()
	defer killCLIProcess(cmd)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		killCLIProcess(cmd)
		runErr = <-done
	}

	duration := time.Since(start)
	if capture.Exceeded() {
		return "", nil, &execution.UnknownOutcomeError{Err: processjob.OutputLimitError()}
	}
	if runErr != nil {
		return "", nil, c.formatExecutionError(capture.StdoutBytes(), capture.StderrString(), runErr, ctx.Err(), duration)
	}

	raw := capture.StdoutBytes()
	if c.name == "claude-cli" && claudeCLIErrorMessage(raw) != "" {
		return "", nil, c.formatExecutionError(raw, capture.StderrString(), errors.New("CLI reported an error"), ctx.Err(), duration)
	}

	// Try structured JSON parsing when the CLI was invoked with a JSON output flag.
	if c.jsonOutput && c.parseJSONResponse != nil {
		content, usage, err := c.parseJSONResponse(raw)
		if err == nil && content != "" {
			if reject, reason := shouldRejectCLIOutput(validationPrompt, content); reject {
				return "", nil, newProviderError(c.provider, "CLI output", ErrorKindConfig, false, 0, reason, nil)
			}
			return content, usage, nil
		}
		// JSON parse failed — fall back to text mode below.
	}

	content := strings.TrimSpace(string(raw))
	content = stripANSI(content)
	if reject, reason := shouldRejectCLIOutput(validationPrompt, content); reject {
		return "", nil, newProviderError(c.provider, "CLI output", ErrorKindConfig, false, 0, reason, nil)
	}
	return content, nil, nil
}

func shouldRejectCLIOutput(prompt, content string) (bool, string) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return true, "empty response from CLI provider"
	}

	lower := strings.ToLower(trimmed)
	if looksLikeCLIAuthPrompt(lower) {
		return true, "provider returned interactive authentication prompt in headless mode"
	}

	if looksLikeCodeOnlyPrompt(prompt) {
		if looksLikePermissionMetaResponse(lower) {
			return true, "provider returned permission/meta response instead of final code output"
		}
		if !containsLikelyCode(trimmed) {
			return true, "provider returned non-code text for code-only request"
		}
	}
	return false, ""
}

func looksLikeCLIAuthPrompt(lower string) bool {
	return strings.Contains(lower, "opening authentication page in your browser") ||
		strings.Contains(lower, "do you want to continue? [y/n]") ||
		strings.Contains(lower, "do you want to continue? [y/n]:") ||
		strings.Contains(lower, "please login") ||
		strings.Contains(lower, "sign in to continue")
}

func looksLikePermissionMetaResponse(lower string) bool {
	return strings.Contains(lower, "could you grant write permission") ||
		strings.Contains(lower, "approve the write permission") ||
		strings.Contains(lower, "file write permission is being denied") ||
		strings.Contains(lower, "the file has been written to") ||
		strings.Contains(lower, "here's a summary of the implementation") ||
		strings.Contains(lower, "it seems write permissions are being blocked") ||
		strings.Contains(lower, "cannot write files in this environment") ||
		strings.Contains(lower, "the listed files are not in") ||
		strings.Contains(lower, "locating the actual project directory") ||
		strings.Contains(lower, "locating the project directory") ||
		strings.Contains(lower, "read the implementation and tests there")
}

func looksLikeCodeOnlyPrompt(prompt string) bool {
	lower := strings.ToLower(prompt)
	if strings.Contains(lower, "--- file:") {
		return true
	}
	if strings.Contains(lower, "complete content of") {
		return true
	}
	fileHints := []string{
		".go", ".js", ".ts", ".tsx", ".py", ".java", ".rs", ".c", ".cpp", ".cs", ".rb", ".php",
	}
	for _, h := range fileHints {
		if strings.Contains(lower, h) {
			// Require at least one strict formatting hint to avoid false positives.
			if strings.Contains(lower, "do not output markdown") ||
				strings.Contains(lower, "no markdown") ||
				strings.Contains(lower, "no explanations") ||
				strings.Contains(lower, "only the complete content") {
				return true
			}
		}
	}
	return false
}

func containsLikelyCode(content string) bool {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "```") || strings.Contains(lower, "--- file:") {
		return true
	}

	for _, line := range strings.Split(content, "\n") {
		l := strings.TrimSpace(strings.ToLower(line))
		if l == "" {
			continue
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
			strings.HasPrefix(l, "return "):
			return true
		}
		// Keep this secondary heuristic strict; bullet summaries often contain
		// braces for examples (for example "{429,503}") but are not code.
		if strings.Contains(l, "=>") {
			return true
		}
	}
	return false
}

func isTransientCLIError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, execution.ErrUnknownOutcome) || stopExecutionReplay(err) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var transient *TransientCLIError
	if errors.As(err, &transient) {
		return true
	}
	if IsRetryableProviderError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, hint := range transientCLIErrorHints {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

func cliRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	return time.Duration(attempt) * cliRetryBaseDelay
}

func formatCLIContextError(provider string, ctxErr error, duration time.Duration) error {
	rounded := duration.Round(time.Second)
	if rounded <= 0 {
		rounded = time.Second
	}

	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return newProviderError(provider, "CLI", ErrorKindTimeout, true, 0, fmt.Sprintf("timeout after %s", rounded), ctxErr)
	case errors.Is(ctxErr, context.Canceled):
		return newProviderError(provider, "CLI", ErrorKindCanceled, false, 0, fmt.Sprintf("canceled after %s", rounded), ctxErr)
	default:
		return newProviderError(provider, "CLI", ErrorKindProvider, false, 0, fmt.Sprintf("context error after %s: %v", rounded, ctxErr), ctxErr)
	}
}

// ClassifyCLIExecutionError shares the single-process CLI outcome classification
// with comparison tools that dispatch a raw vendor process without retries.
func ClassifyCLIExecutionError(provider, stderr string, runErr error, ctxErr error, duration time.Duration) error {
	return formatCLIExecutionError(provider, stderr, runErr, ctxErr, duration)
}

const claudeCLIErrorJSONLimit = 64 << 10
const claudeCLIErrorMessageLimit = 2 << 10
const claudeCLIErrorEnvelopeLimit = processjob.MaxOutputBytes // Complete captured Chat output is also bounded.
const claudeCLIErrorFallback = "Claude CLI reported an error"

var claudeCLIErrorSecrets = regexp.MustCompile(`(?i)\b(?:bearer\s+[^\s"']+|sk-[a-z0-9_-]{8,}|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|password|secret|session[_-]?id|account[_-]?(?:id|ref))["']?\s*[:=]\s*["']?[^\s"',;]+|[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,})`)

// isClaudeCLIErrorEnvelope does not decode diagnostic fields: an oversized or
// wrongly typed result/errors field must not make stream metadata visible.
func isClaudeCLIErrorEnvelope(raw []byte) bool {
	if len(raw) == 0 || len(raw) > claudeCLIErrorEnvelopeLimit {
		return false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var failure struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError *bool  `json:"is_error"`
	}
	if json.Unmarshal(raw, &failure) != nil || (failure.Type != "result" && failure.Type != "error") {
		return false
	}
	if failure.IsError != nil && !*failure.IsError {
		return false
	}
	return failure.IsError != nil || failure.Type == "error" || strings.HasPrefix(failure.Subtype, "error_")
}

// claudeCLIErrorMessage reads only explicit failure envelopes. Claude can put
// these on stdout despite a nonzero exit; the complete envelope also contains
// session/account/usage metadata that must never become an error message.
func claudeCLIErrorMessage(raw []byte) string {
	if !isClaudeCLIErrorEnvelope(raw) {
		return ""
	}
	if len(raw) > claudeCLIErrorJSONLimit {
		return claudeCLIErrorFallback
	}
	var failure struct {
		Result string   `json:"result"`
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(raw, &failure) != nil {
		return claudeCLIErrorFallback
	}
	parts := append([]string{failure.Result}, failure.Errors...)
	var message strings.Builder
	for _, part := range parts {
		part = strings.TrimSpace(strings.Map(func(r rune) rune {
			if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
				return -1
			}
			return r
		}, stripANSI(part)))
		part = claudeCLIErrorSecrets.ReplaceAllString(part, "[redacted]")
		if part == "" {
			continue
		}
		if message.Len() > 0 {
			message.WriteByte('\n')
		}
		remaining := claudeCLIErrorMessageLimit - message.Len()
		if len(part) > remaining {
			part = part[:remaining]
			for !utf8.ValidString(part) {
				part = part[:len(part)-1]
			}
		}
		message.WriteString(part)
		if message.Len() >= claudeCLIErrorMessageLimit {
			break
		}
	}
	if result := strings.TrimSpace(message.String()); result != "" {
		return result
	}
	return claudeCLIErrorFallback
}

func (c *CLIProvider) formatExecutionError(stdout []byte, stderr string, runErr error, ctxErr error, duration time.Duration) error {
	if c.name == "claude-cli" {
		message := claudeCLIErrorMessage(stdout)
		if strings.TrimSpace(stderr) == "" {
			stderr = message
		} else if looksLikeQuotaExhaustion(message) && !looksLikeQuotaExhaustion(stderr) {
			// Keep stderr first, but retain an explicit limit from the failure
			// envelope so a generic CLI warning cannot hide an exhausted pool.
			stderr = strings.TrimSpace(stderr) + "\n" + message
		}
	}
	return formatCLIExecutionError(c.provider, stderr, runErr, ctxErr, duration)
}

func formatCLIExecutionError(provider, stderr string, runErr error, ctxErr error, duration time.Duration) error {
	if ctxErr != nil {
		return formatCLIContextError(provider, ctxErr, duration)
	}
	var exitErr *exec.ExitError
	if errors.Is(runErr, exec.ErrWaitDelay) || (errors.As(runErr, &exitErr) && exitErr.ExitCode() < 0) {
		return &execution.UnknownOutcomeError{Err: fmt.Errorf("%s CLI terminated without a complete exit/output result: %w", provider, runErr)}
	}

	errMsg := strings.TrimSpace(stderr)
	if errMsg == "" {
		errMsg = runErr.Error()
	}
	// Subscription CLIs surface quota exhaustion as a non-zero exit with a
	// usage-limit message on stderr, not an HTTP 429 (that path only exists for
	// the API transports). Classify these as ErrorKindRateLimit so the quota
	// layer seals the pool until its window resets, instead of only tripping the
	// circuit breaker after repeated failures.
	if looksLikeQuotaExhaustion(errMsg) {
		return newProviderError(provider, "CLI", ErrorKindRateLimit, true, 429, errMsg, runErr)
	}
	base := newProviderError(provider, "CLI", ErrorKindProvider, false, 0, errMsg, runErr)
	if looksTransient(errMsg) {
		return &TransientCLIError{Err: newProviderError(provider, "CLI", ErrorKindNetwork, true, 0, errMsg, runErr)}
	}
	return base
}

// quotaExhaustionHints are substrings (lower-cased) that subscription CLIs emit
// when a session/weekly limit is hit. Kept broad on purpose: the exact wording
// is vendor-controlled and undocumented, and a false positive only costs a
// temporary reroute to another pool (recoverable), whereas a miss leaves the
// exhausted pool active. Word-boundary-ish phrases avoid matching unrelated text
// (e.g. a user prompt that merely contains "quota").
var quotaExhaustionHints = []string{
	"usage limit",
	"monthly spend limit",
	"rate limit",
	"quota exceeded",
	"quota exhausted",
	"out of quota",
	"insufficient_quota",
	"too many requests",
	"resets at",
	"limit reached",
	"429",
}

// looksLikeQuotaExhaustion reports whether a CLI error message indicates the
// subscription's quota/rate limit was hit (as opposed to a generic failure).
func looksLikeQuotaExhaustion(msg string) bool {
	lower := strings.ToLower(msg)
	for _, hint := range quotaExhaustionHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// looksTransient checks if an error message matches known transient patterns.
func looksTransient(msg string) bool {
	lower := strings.ToLower(msg)
	for _, hint := range transientCLIErrorHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// buildCLIPrompt converts the message history + system prompt into a single
// text prompt for CLI tools that accept a single prompt string.
func buildCLIPrompt(messages []Message, system string) string {
	var b strings.Builder

	if system != "" {
		b.WriteString(system)
		b.WriteString("\n\n")
	}

	for _, m := range messages {
		switch m.Role {
		case "system":
			// Already handled above, but include additional system messages
			if system == "" {
				b.WriteString(m.Content)
				b.WriteString("\n\n")
			}
		case "user":
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		case "assistant":
			b.WriteString("Previous response:\n")
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		}
	}

	return strings.TrimSpace(b.String())
}

func buildCLIValidationPrompt(messages []Message) string {
	var parts []string
	for _, m := range messages {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if m.Role != "user" {
			continue
		}
		parts = append(parts, m.Content)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// estimateTokens provides a rough token count using a word/rune hybrid.
// It prefers word count for natural-language English and rune count for
// languages/scripts where byte length would significantly overestimate usage.
func estimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	words := len(strings.Fields(text))
	byWords := int(float64(words) * 1.3)
	runes := utf8.RuneCountInString(text)
	byRunes := int(float64(runes) / 3.5)
	if byWords > byRunes {
		return byWords
	}
	return byRunes
}

// stripANSI removes ANSI escape sequences from text.
func stripANSI(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			// Skip ESC sequence
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && (s[i] < 'A' || s[i] > 'Z') && (s[i] < 'a' || s[i] > 'z') {
					i++
				}
				if i < len(s) {
					i++ // skip the final letter
				}
			}
		} else {
			result.WriteByte(s[i])
			i++
		}
	}
	return result.String()
}

// ensureNoProxy ensures NO_PROXY includes Google API hosts.
func ensureNoProxy(env []string) []string {
	const googleHosts = "googleapis.com,google.com,cloudcode-pa.googleapis.com"

	for i, e := range env {
		if strings.HasPrefix(e, "NO_PROXY=") || strings.HasPrefix(e, "no_proxy=") {
			existing := e[len("NO_PROXY="):]
			if !strings.Contains(existing, "googleapis.com") {
				env[i] = e + "," + googleHosts
			}
			return env
		}
	}
	// No NO_PROXY set — add it
	return append(env, "NO_PROXY="+googleHosts, "no_proxy="+googleHosts)
}

// applyGeminiProxyPolicy decides whether Gemini CLI should bypass proxy.
// Default behavior:
//   - if any proxy env var is configured, respect it
//   - otherwise, bypass proxy for Google hosts (historical behavior)
//
// Explicit overrides:
//   - MAKEWAND_GEMINI_USE_PROXY=1   => always keep proxy env untouched
//   - MAKEWAND_GEMINI_BYPASS_PROXY=1 => always append NO_PROXY Google hosts
func applyGeminiProxyPolicy(env []string) []string {
	if envFlagTrue("MAKEWAND_GEMINI_USE_PROXY") {
		return env
	}
	if envFlagTrue("MAKEWAND_GEMINI_BYPASS_PROXY") {
		return ensureNoProxy(env)
	}
	if hasProxyEnv(env) {
		return env
	}
	return ensureNoProxy(env)
}

func envFlagTrue(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func hasProxyEnv(env []string) bool {
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "HTTP_PROXY="):
			return true
		case strings.HasPrefix(e, "HTTPS_PROXY="):
			return true
		case strings.HasPrefix(e, "ALL_PROXY="):
			return true
		case strings.HasPrefix(e, "http_proxy="):
			return true
		case strings.HasPrefix(e, "https_proxy="):
			return true
		case strings.HasPrefix(e, "all_proxy="):
			return true
		}
	}
	return false
}

// filterEnv removes a given env var from the environment list.
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// --- CLI Detection ---

// CLIInfo holds detection results for a CLI tool.
type CLIInfo struct {
	Name    string // "claude", "gemini", "codex"
	BinPath string // resolved path
	Version string // version string
}

// DetectCLIs probes the system for installed subscription CLI tools.
func DetectCLIs() []CLIInfo {
	tools := []struct {
		name     string
		bin      string
		verArgs  []string
		verParse func(output string) string
	}{
		{
			name:    "claude",
			bin:     "claude",
			verArgs: []string{"--version"},
			verParse: func(o string) string {
				// "2.1.63 (Claude Code)"
				return strings.TrimSpace(strings.Split(o, "\n")[0])
			},
		},
		{
			name:    "gemini",
			bin:     "gemini",
			verArgs: []string{"--version"},
			verParse: func(o string) string {
				return strings.TrimSpace(strings.Split(o, "\n")[0])
			},
		},
		{
			name:    "codex",
			bin:     "codex",
			verArgs: []string{"--version"},
			verParse: func(o string) string {
				return strings.TrimSpace(strings.Split(o, "\n")[0])
			},
		},
	}

	var results []CLIInfo
	for _, t := range tools {
		binPath, err := exec.LookPath(t.bin)
		if err != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, binPath, t.verArgs...).Output()
		cancel()

		version := "unknown"
		if err == nil && len(out) > 0 {
			version = t.verParse(string(out))
		}

		results = append(results, CLIInfo{
			Name:    t.name,
			BinPath: binPath,
			Version: version,
		})
	}

	return results
}

// DetectCLIsJSON returns detected CLIs as JSON (for debugging).
func DetectCLIsJSON() string {
	clis := DetectCLIs()
	data, _ := json.MarshalIndent(clis, "", "  ")
	return string(data)
}
