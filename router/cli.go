package router

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
	"github.com/makewand/makewand/internal/proxyconfig"
)

// Test seam for a transient ledger failure after a supervised worker starts.
var cliRegisterJobWorker = (*execution.Attempt).RegisterJobWorker

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
		return cmd
	}
	p.buildStreamCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		args := []string{"-p", prompt, "--sandbox", "false", "--output-format", "stream-json"}
		if modelID, ok := ModelFromContext(ctx); ok && !strings.HasSuffix(modelID, "-cli") {
			args = append(args, "--model", modelID)
		}
		cmd := exec.CommandContext(ctx, binPath, args...)
		return cmd
	}
	p.checkCmd = func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binPath, "--version")
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
	defaults, proxyErr := proxyconfig.Load()
	if proxyErr != nil {
		return false
	}
	cmd.Env = proxyconfig.Apply(cmd.Environ(), defaults)
	if c.provider == "gemini" || c.provider == "agy" {
		cmd.Env = applyGeminiProxyPolicy(cmd.Env)
	}
	setCLIProcessGroup(cmd)
	capture := processjob.NewCapture(processjob.MaxOutputBytes, func() { killCLIProcess(cmd) })
	cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
	cleanupProcess, err := processjob.StartSandboxed(cmd)
	if err != nil {
		cliAfterWait(cmd) // nothing ran: a private home is removed at once
		// The deadline can pass while a slow host is still starting the probe
		// (a fresh binary being scanned): that is the same unknown availability
		// as a probe that started and stalled.
		return errors.Is(ctx.Err(), context.DeadlineExceeded) && c.softPassProbeTimeout()
	}
	cliAfterStart(cmd)
	defer cleanupProcess()
	defer killCLIProcess(cmd)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	available := c.waitHealthProbe(ctx, done, capture, func() { killCLIProcess(cmd) })
	// A probe run in a sandbox with a private provider home (a probe command
	// wrapped by wrapCLICommandWithSandbox) may refresh tokens too: they are
	// on the host before the probe reports (bounded, see cliAfterWait).
	cliAfterWait(cmd)
	return available
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
	// A cancellation ends the stream once the CLI's own stream has ended,
	// after its sandbox's credential write-back (streamEndWait).
	return observeDispatchStream(ctx, stream, attempt, c.streamEndWait()), nil
}

func (c *CLIProvider) chatStreamUnaccounted(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	lifecycle, _ := ctx.Value(providerLifecycleKey{}).(*execution.Attempt)
	if err := initializeCLIEvidence(lifecycle, c.provider); err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("%w: %w", errExecutionAdmission, err)
	}
	readOnly := cliRunReadOnly(ctx, wrappedCmd != cmd)
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
	attempt, _ := ctx.Value(providerLifecycleKey{}).(*execution.Attempt)
	if err := attempt.BeginLaunch(); err != nil {
		// A failed fsync can follow a committed fence; no child has started.
		err = errors.Join(err, attempt.LaunchFailed())
		_ = stdoutPipe.Close()
		_ = stdoutWriter.Close()
		cancel()
		cleanupWorkDir()
		cliAfterWait(cmd)
		return nil, err
	}
	cleanupProcess, err := processjob.StartSandboxed(cmd)
	if err != nil {
		err = errors.Join(err, attempt.LaunchFailed())
		cliAfterWait(cmd) // nothing ran: the private home is removed at once
		contextErr := ctx.Err()
		_ = stdoutPipe.Close()
		_ = stdoutWriter.Close()
		cancel()
		cleanupWorkDir()
		if contextErr != nil {
			return nil, c.formatExecutionError(readOnly, capture.StdoutBytes(), capture.StderrString(), prompt, err, errors.Join(contextErr, err), time.Since(started))
		}
		return nil, newProviderError(c.provider, "CLI start", ErrorKindConfig, false, 0, err.Error(), err)
	}
	cliAfterStart(cmd)
	if err := cliRegisterJobWorker(attempt, cmd.Process.Pid); err != nil {
		killCLIProcess(cmd)
		_ = cmd.Wait()
		cliAfterWait(cmd)
		cleanupProcess()
		err = errors.Join(err, attempt.WorkerStopped())
		_ = stdoutPipe.Close()
		_ = stdoutWriter.Close()
		cancel()
		cleanupWorkDir()
		return nil, err
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer func() { _ = attempt.WorkerStopped() }()
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
			cleanupProcess()
			_ = stdoutWriter.Close()
			// The outcome, and with it the terminal chunk, waits for the
			// sandbox's credential write-back (bounded, see cliAfterWait):
			// a refreshed token is on the host before the stream ends.
			cliAfterWait(cmd)
			recordCLIExit(attempt, cmd)
			if c.parseStreamLine != nil {
				recordCLIOutput(attempt, capture.StdoutBytes())
			}
			waitErr = errors.Join(waitErr, verifyDirectCLIAccount(cmd), attempt.WorkerStopped())
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
			ch <- StreamChunk{Error: c.formatExecutionError(readOnly, stdout, capture.StderrString(), prompt, waitErr, nil, duration)}
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
	attempt, _ := ctx.Value(providerLifecycleKey{}).(*execution.Attempt)
	if err := initializeCLIEvidence(attempt, c.provider); err != nil {
		return "", nil, err
	}
	cmd := c.buildCmd(ctx, prompt)
	applyCLIWorkDir(ctx, cmd, scratchDir)
	wrappedCmd, err := wrapCLICommandWithSandbox(ctx, c.provider, cmd)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", errExecutionAdmission, err)
	}
	readOnly := cliRunReadOnly(ctx, wrappedCmd != cmd)
	cmd = wrappedCmd
	setCLIProcessGroup(cmd)

	capture := processjob.NewCapture(processjob.MaxOutputBytes, func() { killCLIProcess(cmd) })
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()

	start := time.Now()
	if err := attempt.BeginLaunch(); err != nil {
		err = errors.Join(err, attempt.LaunchFailed())
		cliAfterWait(cmd)
		return "", nil, err
	}
	cleanupProcess, err := processjob.StartSandboxed(cmd)
	if err != nil {
		err = errors.Join(err, attempt.LaunchFailed())
		cliAfterWait(cmd) // nothing ran: the private home is removed at once
		if contextErr := ctx.Err(); contextErr != nil {
			return "", nil, c.formatExecutionError(readOnly, capture.StdoutBytes(), capture.StderrString(), prompt, err, errors.Join(contextErr, err), time.Since(start))
		}
		return "", nil, newProviderError(c.provider, "CLI start", ErrorKindConfig, false, 0, err.Error(), err)
	}
	cliAfterStart(cmd)
	if err := cliRegisterJobWorker(attempt, cmd.Process.Pid); err != nil {
		killCLIProcess(cmd)
		_ = cmd.Wait()
		cliAfterWait(cmd)
		cleanupProcess()
		return "", nil, errors.Join(err, attempt.WorkerStopped())
	}
	defer func() { _ = attempt.WorkerStopped() }()
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
	killCLIProcess(cmd)
	cleanupProcess()
	runErr = errors.Join(runErr, attempt.WorkerStopped())

	duration := time.Since(start)
	// The sandbox has exited: its credential write-back lands on the host
	// before the attempt returns (bounded, see cliAfterWait), so the caller,
	// a retry and the next run all see a refreshed token.
	cliAfterWait(cmd)
	runErr = errors.Join(runErr, verifyDirectCLIAccount(cmd))
	recordCLIExit(attempt, cmd)
	if c.jsonOutput && (c.provider != "codex" || !codexUsesReviewSubcommand(ctx)) {
		recordCLIOutput(attempt, capture.StdoutBytes())
	}

	if capture.Exceeded() {
		return "", nil, &execution.UnknownOutcomeError{Err: processjob.OutputLimitError()}
	}
	if runErr != nil {
		return "", nil, c.formatExecutionError(readOnly, capture.StdoutBytes(), capture.StderrString(), prompt, runErr, ctx.Err(), duration)
	}

	raw := capture.StdoutBytes()
	if c.name == "claude-cli" && claudeCLIErrorMessage(raw) != "" {
		return "", nil, c.formatExecutionError(readOnly, raw, capture.StderrString(), prompt, errors.New("CLI reported an error"), ctx.Err(), duration)
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
// stdout and stderr are what the process printed. The process is taken to be
// one that could have written its working directory: the CLI's own refusal of
// the invocation is a known failure only when nothing else it printed can show
// work, and otherwise the outcome stays unknown.
func ClassifyCLIExecutionError(provider, stdout, stderr string, runErr error, ctxErr error, duration time.Duration) error {
	return formatCLIExecutionError(provider, stdout, stderr, runErr, ctxErr, duration)
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

// formatExecutionError classifies a failed run of c's CLI. prompt is the
// prompt as sent: codex echoes it into its transcript, and the echo is never
// its own message. readOnly tells whether the run could not have written the
// workspace (cliRunReadOnly).
func (c *CLIProvider) formatExecutionError(readOnly bool, stdout []byte, stderr, prompt string, runErr error, ctxErr error, duration time.Duration) error {
	if err := cliOutcomeError(c.provider, runErr, ctxErr, duration); err != nil {
		return err
	}
	cli := c.messageCatalogName()
	quota := cliQuotaRefusal(cli, stderr, prompt)
	// A run that could have written the workspace is replayed elsewhere only on
	// a refusal that is all it shows (cliInvocationRefusal): a login lost
	// mid-run, after the model edited files, is in the same words as one refused
	// at the start.
	answered := cliAnswered(string(stdout))
	if c.name == "claude-cli" {
		message := claudeCLIErrorMessage(stdout)
		if strings.TrimSpace(stderr) == "" {
			stderr, quota = message, cliQuotaRefusal(cli, message, prompt)
		} else if !quota && cliQuotaRefusal(cli, message, prompt) {
			// Keep stderr first, but retain an explicit limit from the failure
			// envelope so a generic CLI warning cannot hide an exhausted pool.
			stderr, quota = strings.TrimSpace(stderr)+"\n"+message, true
		}
	}
	var refusal cliRefusal
	if readOnly || !answered {
		refusal = cliInvocationRefusal(cli, stderr, prompt, !readOnly)
	}
	err := cliFailureError(c.provider, stderr, quota, refusal, runErr)
	if readOnly {
		return readOnlyExitError(err)
	}
	return err
}

// formatCLIExecutionError classifies a failed run of provider's CLI from what
// it printed (ClassifyCLIExecutionError). The run could have written its
// working directory, so the CLI's own refusal counts only as it does in such a
// routed run: when it is all the run shows.
func formatCLIExecutionError(provider, stdout, stderr string, runErr error, ctxErr error, duration time.Duration) error {
	if err := cliOutcomeError(provider, runErr, ctxErr, duration); err != nil {
		return err
	}
	var refusal cliRefusal
	if !cliAnswered(stdout) {
		refusal = cliInvocationRefusal(cliCatalogName(provider), stderr, "", true)
	}
	return cliFailureError(provider, stderr, looksLikeQuotaExhaustion(provider, stderr), refusal, runErr)
}

// cliAnswered reports whether a CLI printed more than blank lines on stdout,
// where its answer, or the report of work it did, goes.
func cliAnswered(stdout string) bool {
	return pyStrip(stripCLIANSI(stdout)) != ""
}

// cliOutcomeError is the error of a run that ended without a complete exit
// result (context done, killed, pipes held open), nil for a CLI failure.
func cliOutcomeError(provider string, runErr error, ctxErr error, duration time.Duration) error {
	if ctxErr != nil {
		return formatCLIContextError(provider, ctxErr, duration)
	}
	var exitErr *exec.ExitError
	if errors.Is(runErr, exec.ErrWaitDelay) || (errors.As(runErr, &exitErr) && exitErr.ExitCode() < 0) {
		return &execution.UnknownOutcomeError{Err: fmt.Errorf("%s CLI terminated without a complete exit/output result: %w", provider, runErr)}
	}
	return nil
}

// cliFailureError is the error of a CLI that exited with a failure, read from
// its stderr. quota tells whether stderr carries the CLI's own quota or
// rate-limit refusal, refusal its own refusal of the invocation.
func cliFailureError(provider, stderr string, quota bool, refusal cliRefusal, runErr error) error {
	errMsg := strings.TrimSpace(stderr)
	if errMsg == "" {
		errMsg = runErr.Error()
	}
	// Subscription CLIs surface quota exhaustion as a non-zero exit with a
	// usage-limit message on stderr, not an HTTP 429 (that path only exists for
	// the API transports). Classify these as ErrorKindRateLimit so the quota
	// layer seals the pool until its window resets, instead of only tripping the
	// circuit breaker after repeated failures. Only provider's own message
	// counts, read from stderr as the CLI wrote it (indentation included).
	if quota {
		return newProviderError(provider, "CLI", ErrorKindRateLimit, true, 429, errMsg, runErr)
	}
	// A CLI that refused the invocation itself (no login, arguments it rejects)
	// did no model work: a known FAILED, as health.py reports it, which the
	// router may replay on the next provider. A run whose outcome is unknown
	// already (a login switch during it) stays unknown.
	if refusal.kind != "" && !errors.Is(runErr, execution.ErrUnknownOutcome) {
		err := newProviderError(provider, "CLI", refusal.kind, false, 0, refusal.line, runErr)
		return &execution.StatusError{Status: execution.Failed, OutcomeKnown: true, Err: err}
	}
	base := newProviderError(provider, "CLI", ErrorKindProvider, false, 0, errMsg, runErr)
	if looksTransient(errMsg) {
		return &TransientCLIError{Err: newProviderError(provider, "CLI", ErrorKindNetwork, true, 0, errMsg, runErr)}
	}
	return base
}

// --- Provider CLI messages ---
//
// A quota or rate-limit refusal seals a pool only when the provider CLI printed
// it itself, at the start of a line that CLI writes. The same words in the
// model's answer, in tool output a codex transcript relays ("Error: 429 Too Many
// Requests", "HTTP/2 429", "429 passed") or in prose are task content: sealing a
// pool on them would reroute healthy work until the pool's window resets.
//
// What each CLI prints, and which of its lines count, is data:
// makewand/cli_message_catalog.json (its "_doc" describes the format), which
// makewand/health.py reads and cli_message_catalog.json here mirrors byte for
// byte. findCLIMessages is health.py's find_cli_messages with the same line
// selection, and cliCatalogRegexp compiles each pattern to match what Python's
// re matches, so both sides reach the same verdict on the same output
// (tests/testdata/cli_message_parity.json, checked by Go and Python tests).

//go:embed cli_message_catalog.json
var cliMessageCatalogJSON []byte

var cliMessages = mustLoadCLIMessageCatalog(cliMessageCatalogJSON)

// quotaExhaustionKinds are the catalog kinds that seal a pool: a hard usage,
// spend or credit limit ("quota") and a request-rate refusal ("rate"). Server
// capacity refusals ("overload", Claude's 529) are transient, not exhaustion.
var quotaExhaustionKinds = []string{"quota", "rate"}

// looksLikeQuotaExhaustion reports whether stderr, captured apart from the
// answer, carries a quota, usage-limit or request-rate refusal that provider's
// CLI printed itself (see cliQuotaRefusal). Another CLI's formats never count.
func looksLikeQuotaExhaustion(provider, stderr string) bool {
	return cliQuotaRefusal(cliCatalogName(provider), stderr, "")
}

// cliQuotaRefusal reports whether stderr carries a quota or rate refusal of
// cli: health.py's find_cli_messages(<cli>, ("quota", "rate"), "", stderr,
// prompt=prompt). A CLI the catalog does not know is read with the generic
// entry (cliGenericEntry).
func cliQuotaRefusal(cli, stderr, prompt string) bool {
	entry := cliMessages.providers[cli]
	if entry == nil {
		entry = cliGenericEntry(cli)
	}
	streams := cliStreams{stderr: stderr, apart: true}
	return len(cliMessages.find(entry, quotaExhaustionKinds, streams, prompt)) > 0
}

// cliRefusalKinds are the catalog kinds of a CLI's refusal to run at all: a
// login it requires or credentials it rejects ("auth").
var cliRefusalKinds = []string{"auth"}

// agy prints these notices on every start, before its silent keyring sign-in,
// and they are moot once that sign-in succeeded (makewand/providers/agy.py
// _AGY_AUTH_OK_RE and _AGY_STARTUP_AUTH_NOTICE_RE).
var (
	agyAuthSucceeded     = regexp.MustCompile(`(?i)authenticated (?:successfully|via )`)
	agyStartupAuthNotice = regexp.MustCompile(`(?i)^(?:you are not logged into antigravity\.?|print mode: not authenticated, trying silent auth)$`)
)

// cliRefusal is a CLI's own refusal of the invocation: its kind ("" for none)
// and the line that shows it.
type cliRefusal struct {
	kind ErrorKind
	line string
}

// cliInvocationRefusal returns cli's own refusal of the invocation in stderr,
// which shows that no model work was done. A rejection of its arguments (agy's
// invalid_invocation) is ErrorKindConfig, a login refusal (a catalog "auth"
// message) ErrorKindAuth: what makewand/providers/agy.py, grok.py and muse.py
// read as a known failure. Only cli's own lines count, as for
// cliQuotaRefusal; a CLI the catalog does not know refuses nothing. An
// argument rejection counts only as the first line the CLI wrote itself
// (firstOwnLine).
//
// A run that could have written the workspace (writable) refuses only when
// the refusal is the first line that may show the CLI at work (firstWorkLine),
// and the caller checks that stdout holds no answer either. The catalog's
// login messages include those of a session that expires while the model
// works, agy logs its tool and model calls as glog diagnostics, and a tool the
// model ran can print an argument rejection of its own.
func cliInvocationRefusal(cli, stderr, prompt string, writable bool) cliRefusal {
	entry := cliMessages.providers[cli]
	if entry == nil {
		return cliRefusal{}
	}
	first := cliMessages.firstOwnLine
	if writable {
		first = cliMessages.firstWorkLine
	}
	// An argument rejection counts only as that first line (makewand/providers/
	// agy.py _agy_invocation_rejection on _agy_user_facing); the same words
	// further on (the answer, a tool's output) are task content.
	if line := normalizeCLILine(first(entry, stderr)); line != "" && entry.invalidInvocation != nil && entry.invalidInvocation.MatchString(line) {
		return cliRefusal{ErrorKindConfig, line}
	}
	if entry.kinds["auth"] == nil && entry.kinds["auth.marked"] == nil {
		return cliRefusal{}
	}
	if cli == "agy" && agyAuthSucceeded.MatchString(stderr) {
		var kept []string
		for _, line := range pySplitLines(stderr) {
			if !agyStartupAuthNotice.MatchString(normalizeCLILine(line)) {
				kept = append(kept, line)
			}
		}
		stderr = strings.Join(kept, "\n")
	}
	line, ok := cliMessages.find(entry, cliRefusalKinds, cliStreams{stderr: stderr, apart: true}, prompt)["auth"]
	if !ok {
		return cliRefusal{}
	}
	if writable {
		work := cliStreams{stderr: first(entry, stderr), apart: true}
		if _, ok := cliMessages.find(entry, cliRefusalKinds, work, prompt)["auth"]; !ok {
			return cliRefusal{}
		}
	}
	return cliRefusal{ErrorKindAuth, line}
}

// firstOwnLine is the first line the CLI of entry wrote itself in own, as
// written: glog diagnostics, blank lines, its status lines and notices are not
// such lines (makewand/providers/agy.py _agy_user_facing).
func (c *cliMessageCatalog) firstOwnLine(entry *cliCatalogEntry, own string) string {
	lines := pySplitLines(own)
	skipped := c.glogEntryLines(lines, false)
	for i, raw := range lines {
		if skipped[i] || pyStrip(stripCLIANSI(raw)) == "" {
			continue
		}
		if line := normalizeCLILine(raw); entry.notices != nil && entry.notices.MatchString(line) || entry.status != nil && entry.status.MatchString(line) {
			continue
		}
		return raw
	}
	return ""
}

// firstWorkLine is the first line in own, as written, that may show the CLI of
// entry at work: any but a blank line and one of its notices (agy's notices and
// generic_notice: warnings about its flags, environment and credentials, none
// of them the trace of a tool or model call). Unlike firstOwnLine it skips no
// glog diagnostics, in which agy logs its tool and model calls, and no status
// line, which agy prints about the background tasks of a root agent that
// already ran.
func (c *cliMessageCatalog) firstWorkLine(entry *cliCatalogEntry, own string) string {
	for _, raw := range pySplitLines(own) {
		if pyStrip(stripCLIANSI(raw)) == "" || entry.notices != nil && entry.notices.MatchString(normalizeCLILine(raw)) {
			continue
		}
		return raw
	}
	return ""
}

// cliCatalogName is the catalog entry of a router provider or CLI name: the
// entry of that name or of the CLI that prints that name for itself
// ("antigravity"). Any other name is returned as is.
func cliCatalogName(provider string) string {
	if _, ok := cliMessages.providers[provider]; ok {
		return provider
	}
	if name, ok := cliMessages.names[provider]; ok {
		return name
	}
	return provider
}

// messageCatalogName is the catalog entry of the CLI c runs. A built-in
// transport reads its own CLI's entry: agy (NewAgyCLI) and the older Gemini
// CLI (NewGeminiCLI) both serve the router name "gemini" and print different
// messages. A custom command reads the entry of its provider name.
func (c *CLIProvider) messageCatalogName() string {
	if cli, ok := strings.CutSuffix(c.name, "-cli"); ok {
		if _, known := cliMessages.providers[cli]; known {
			return cli
		}
	}
	return cliCatalogName(c.provider)
}

// cliGenericKinds are the refusals of a CLI the catalog does not know (a
// custom command): the phrases a quota or rate-limit error starts with, in
// the catalog's dialect. As for a catalog CLI they count only at the start of
// a message, and only in the error report the CLI ends with.
//
// They are not in cli_message_catalog.json, the source both engines share,
// because only this engine needs them: custom commands (NewCommandCLI) are a
// router feature, and the Python engine runs only the CLIs the catalog
// describes (for any other name health.find_cli_messages finds nothing).
// Patterns no Python code reads would be untested by the shared parity corpus
// and would make the catalog say more than both engines do.
var cliGenericKinds = map[string][]string{
	"quota": {
		`(?:(?:usage|spend|spending|credit|token|request|hourly|daily|weekly|monthly|session|5-hour|five-hour|plan|account|api) ){1,2}limit (?:reached|exceeded)\b`,
		`limit (?:reached|exceeded)(?:$|[.!:;,·•∙|(]| -)`,
		`(?:(?:usage|api|hourly|daily|weekly|monthly|project|individual|account|plan|token|request) )?quota (?:exceeded|exhausted|reached)\b`,
		`insufficient[_ ]quota\b`,
		`(?:you(?:'re| are) )?out of (?:quota|credits?|usage(?: credits)?)\b`,
		`you(?:'ve| have) (?:hit|reached|exceeded|exhausted|used up) (?:your|the) (?:[\w'$-]+ ){0,4}?(?:limit|quota|budget|cap|credits)\b`,
		`resource[_ ]?exhausted\b`,
	},
	"rate": {
		`(?:[\w-]+\.)*rate[ _-]?limit(?:ed\b|[ _-]?(?:exceeded|reached|hit|error)\b)`,
		`too many requests\b`,
		`(?:http|status|error)(?: code)?[ :]+429\b`,
		`429(?:$|:| too many requests\b)`,
	},
}

// cliGeneric is the compiled generic entry, without a CLI name.
var cliGeneric = mustCompileCLIGenericEntry()

func mustCompileCLIGenericEntry() *cliCatalogEntry {
	entry := &cliCatalogEntry{
		ownStreams: []string{"stderr"},
		selection:  map[string]string{"apart": "final_report", "merged": "final_report"},
		kinds:      map[string]*regexp.Regexp{},
	}
	for kind, patterns := range cliGenericKinds {
		re, err := cliCatalogRegexp(cliAlternation(patterns))
		if err != nil {
			panic("router: generic CLI messages: " + err.Error())
		}
		entry.kinds[kind] = re
	}
	return entry
}

// cliGenericEntry is the generic entry for the CLI named name, which it may
// print in front of its lines ("name: quota exceeded").
func cliGenericEntry(name string) *cliCatalogEntry {
	entry := *cliGeneric
	if name != "" {
		entry.own, _ = cliCatalogRegexp("(?:" + regexp.QuoteMeta(name) + ")" + cliMessages.nameSeparator)
	}
	return &entry
}

// cliStreams is what a CLI printed: one merged stream (output), or stdout
// (output) and stderr captured apart.
type cliStreams struct {
	output, stderr string
	apart          bool
}

// cliLine is a line a CLI may have written itself (health.py _cli_lines).
type cliLine struct {
	message string // where its message starts
	line    string // the normalized line
	marked  bool   // an error marker leads the message
	chain   bool   // an entry of an anyhow "Caused by:" chain
}

// cliMessageCatalog is the compiled catalog.
type cliMessageCatalog struct {
	glogHeader, tracing, causedBy, causedByEntry, topLevel, marker, markedBracket, leadIn *regexp.Regexp

	nameSeparator                           string
	tracingLevel, tracingTarget             int // subexpressions of tracing
	markerGlyph, markerBracket, markerError int // subexpressions of marker

	providers map[string]*cliCatalogEntry
	names     map[string]string // a name a CLI prints in front of its lines -> its entry
}

// cliCatalogEntry is one provider CLI of the catalog.
type cliCatalogEntry struct {
	ownStreams  []string
	selection   map[string]string // "apart"/"merged" -> "lines", "final_report" or "transcript"
	transcript  *cliTranscript
	kinds       map[string]*regexp.Regexp // "quota", "quota.marked", ...: one alternation each
	own         *regexp.Regexp            // its names as it prints them in front of its lines
	jsonReports bool                      // a JSON object it prints over several lines is one line
	// notices: its own notices, generic ones included (agy's notices and
	// generic_notice); status: its status lines (status_lines);
	// invalidInvocation: its rejections of its arguments. nil when the catalog
	// lists none.
	notices, status, invalidInvocation *regexp.Regexp
}

// cliTranscript is codex exec's transcript structure (providers.codex.transcript).
type cliTranscript struct {
	banner, section, promptEcho, execResult, errorEvent, ownError, tracingTarget *regexp.Regexp
	fatalError, fatalAfter                                                       *regexp.Regexp // nil: no fatal report
}

func mustLoadCLIMessageCatalog(data []byte) *cliMessageCatalog {
	catalog, err := loadCLIMessageCatalog(data)
	if err != nil {
		panic("router: cli_message_catalog.json: " + err.Error())
	}
	return catalog
}

func loadCLIMessageCatalog(data []byte) (*cliMessageCatalog, error) {
	var file struct {
		Lines     map[string]string `json:"lines"`
		Providers map[string]struct {
			Names         []string          `json:"names"`
			OwnStreams    []string          `json:"own_streams"`
			AnswerStreams []string          `json:"answer_streams"`
			Selection     map[string]string `json:"selection"`
			Transcript    *struct {
				Banner        string   `json:"banner"`
				Section       string   `json:"section"`
				PromptEcho    string   `json:"prompt_echo"`
				ExecResult    string   `json:"exec_result"`
				ErrorEvent    string   `json:"error_event"`
				OwnError      []string `json:"own_error"`
				TracingTarget string   `json:"tracing_target"`
				FatalError    string   `json:"fatal_error"`
				FatalAfter    string   `json:"fatal_after"`
			} `json:"transcript"`
			Kinds             map[string][]string `json:"kinds"`
			JSONReports       any                 `json:"json_reports"`
			Notices           []string            `json:"notices"`
			GenericNotice     string              `json:"generic_notice"`
			StatusLines       []string            `json:"status_lines"`
			InvalidInvocation []string            `json:"invalid_invocation"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	compile := func(name, pattern string) *regexp.Regexp {
		if pattern == "" {
			fail(fmt.Errorf("%s: missing", name))
			return nil
		}
		re, err := cliCatalogRegexp(pattern)
		if err != nil {
			fail(fmt.Errorf("%s: %w", name, err))
		}
		return re
	}
	line := func(name string) *regexp.Regexp { return compile("lines."+name, file.Lines[name]) }
	c := &cliMessageCatalog{
		glogHeader:    line("glog_header"),
		tracing:       line("tracing"),
		causedBy:      line("caused_by"),
		causedByEntry: line("caused_by_entry"),
		topLevel:      line("top_level"),
		marker:        line("marker"),
		markedBracket: line("marked_bracket"),
		leadIn:        line("lead_in"),
		nameSeparator: file.Lines["name_separator"],
		providers:     map[string]*cliCatalogEntry{},
		names:         map[string]string{},
	}
	if firstErr != nil {
		return nil, firstErr
	}
	subexp := func(re *regexp.Regexp, name string) int {
		index := re.SubexpIndex(name)
		if index < 0 {
			fail(fmt.Errorf("lines: no (?P<%s>...) group", name))
		}
		return index
	}
	c.tracingLevel, c.tracingTarget = subexp(c.tracing, "level"), subexp(c.tracing, "target")
	c.markerGlyph, c.markerBracket, c.markerError = subexp(c.marker, "glyph"), subexp(c.marker, "bracket"), subexp(c.marker, "error")
	// optional compiles a pattern the catalog may leave out.
	optional := func(name, pattern string) *regexp.Regexp {
		if pattern == "" {
			return nil
		}
		return compile(name, pattern)
	}
	for name, spec := range file.Providers {
		prefix := "providers." + name + "."
		if err := checkCLISelectionStreams(spec.OwnStreams, spec.AnswerStreams, spec.Selection); err != nil {
			fail(fmt.Errorf("%s%w", prefix, err))
		}
		entry := &cliCatalogEntry{ownStreams: spec.OwnStreams, selection: spec.Selection, kinds: map[string]*regexp.Regexp{},
			jsonReports: spec.JSONReports == true} // health.py: entry.get("json_reports") is True
		for kind, patterns := range spec.Kinds {
			if len(patterns) > 0 {
				entry.kinds[kind] = compile(prefix+"kinds."+kind, cliAlternation(patterns))
			}
		}
		// makewand/providers/agy.py _agy_own_line(any_warning=True) and
		// _agy_invocation_rejection read these lists.
		notices := slices.Clone(spec.Notices)
		if spec.GenericNotice != "" {
			notices = append(notices, spec.GenericNotice)
		}
		if len(notices) > 0 {
			entry.notices = compile(prefix+"notices", cliAlternation(notices))
		}
		if len(spec.StatusLines) > 0 {
			entry.status = compile(prefix+"status_lines", cliAlternation(spec.StatusLines))
		}
		if len(spec.InvalidInvocation) > 0 {
			entry.invalidInvocation = compile(prefix+"invalid_invocation", cliAlternation(spec.InvalidInvocation))
		}
		if t := spec.Transcript; t != nil {
			entry.transcript = &cliTranscript{
				banner:        compile(prefix+"transcript.banner", t.Banner),
				section:       compile(prefix+"transcript.section", t.Section),
				promptEcho:    compile(prefix+"transcript.prompt_echo", t.PromptEcho),
				execResult:    compile(prefix+"transcript.exec_result", t.ExecResult),
				errorEvent:    compile(prefix+"transcript.error_event", t.ErrorEvent),
				ownError:      compile(prefix+"transcript.own_error", cliAlternation(t.OwnError)),
				tracingTarget: compile(prefix+"transcript.tracing_target", t.TracingTarget),
				fatalError:    optional(prefix+"transcript.fatal_error", t.FatalError),
				fatalAfter:    optional(prefix+"transcript.fatal_after", t.FatalAfter),
			}
		}
		if len(spec.Names) > 0 {
			// health.py _cli_name_regex: (?:<name>|...)<name_separator>.
			quoted := make([]string, len(spec.Names))
			for i, cliName := range spec.Names {
				quoted[i] = regexp.QuoteMeta(cliName)
				if other, taken := c.names[cliName]; taken && other != name {
					fail(fmt.Errorf("%snames: %q is also %s's", prefix, cliName, other))
				}
				c.names[cliName] = name
			}
			entry.own = compile(prefix+"names", "(?:"+strings.Join(quoted, "|")+")"+c.nameSeparator)
		}
		c.providers[name] = entry
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return c, nil
}

// checkCLISelectionStreams checks a catalog entry's streams against its
// selection: own and answer streams are stdout or stderr, and the selection
// 'lines' (every candidate line counts, also where a mode has no selection)
// never reads a capture that holds the model's answer (answer streams): a
// merged capture holds every stream, one captured apart the CLI's own
// streams. A quota word in the answer would otherwise seal a pool.
func checkCLISelectionStreams(own, answer []string, selection map[string]string) error {
	for _, stream := range slices.Concat(own, answer) {
		if stream != "stdout" && stream != "stderr" {
			return fmt.Errorf("streams: unknown stream %q", stream)
		}
	}
	lines := func(mode string) bool {
		s, ok := selection[mode]
		return !ok || s == "lines"
	}
	if lines("merged") && len(answer) > 0 {
		return fmt.Errorf("selection.merged: 'lines' would read the model's answer (answer_streams %q)", answer)
	}
	if lines("apart") {
		for _, stream := range own {
			if slices.Contains(answer, stream) {
				return fmt.Errorf("selection.apart: 'lines' would read the model's answer on %s, an own stream that is also an answer stream", stream)
			}
		}
	}
	return nil
}

// cliAlternation joins patterns the way health.py does: (?:p1)|(?:p2)|...
func cliAlternation(patterns []string) string {
	if len(patterns) == 0 {
		return ""
	}
	return "(?:" + strings.Join(patterns, ")|(?:") + ")"
}

// findCLIMessages returns, for each of kinds, the first (normalized) line on
// which provider's CLI prints one of its own messages of that kind: health.py
// find_cli_messages. provider is a catalog entry; an unknown CLI prints none.
// prompt, the prompt as sent, lets a codex transcript skip its echo.
func findCLIMessages(provider string, kinds []string, streams cliStreams, prompt string) map[string]string {
	entry := cliMessages.providers[provider]
	if entry == nil {
		return map[string]string{}
	}
	return cliMessages.find(entry, kinds, streams, prompt)
}

// find is findCLIMessages for the CLI of entry.
func (c *cliMessageCatalog) find(entry *cliCatalogEntry, kinds []string, streams cliStreams, prompt string) map[string]string {
	found := map[string]string{}
	type kindPatterns struct {
		kind               string
		standalone, marked *regexp.Regexp
	}
	patterns := make([]kindPatterns, len(kinds))
	for i, kind := range kinds {
		patterns[i] = kindPatterns{kind, entry.kinds[kind], entry.kinds[kind+".marked"]}
	}
	for _, selected := range c.selectedLines(entry, streams, prompt) {
		complete := !c.messageStarts(selected.message, selected.marked, entry.own, func(start string, marked bool) bool {
			for _, p := range patterns {
				if _, ok := found[p.kind]; ok {
					continue
				}
				if (p.standalone != nil && p.standalone.MatchString(start)) ||
					(marked && p.marked != nil && p.marked.MatchString(start)) {
					found[p.kind] = selected.line
				}
			}
			return len(found) < len(patterns)
		})
		if complete {
			break
		}
	}
	return found
}

// selectedLines picks the lines of streams that entry's CLI wrote itself, by
// the catalog's selection for how the streams were captured (health.py
// _cli_selected_lines).
func (c *cliMessageCatalog) selectedLines(entry *cliCatalogEntry, streams cliStreams, prompt string) []cliLine {
	mode := "merged"
	if streams.apart {
		mode = "apart"
	}
	selection, ok := entry.selection[mode]
	if !ok {
		selection = "lines"
	}
	if selection == "final_report" {
		// Every stream ends with its own last lines: with the streams captured
		// apart, the report each of the CLI's own streams ends with counts.
		parts := []string{streams.output}
		if streams.apart {
			parts = parts[:0]
			if slices.Contains(entry.ownStreams, "stdout") {
				parts = append(parts, streams.output)
			}
			if slices.Contains(entry.ownStreams, "stderr") {
				parts = append(parts, streams.stderr)
			}
		}
		var report []cliLine
		for _, part := range parts {
			report = append(report, c.finalReport(c.lines(c.tail(part, nil, ""), entry, nil, ""), entry.own)...)
		}
		return report
	}
	var transcript *cliTranscript
	echo := "" // only a transcript echoes the prompt
	if selection == "transcript" {
		transcript, echo = entry.transcript, prompt
	}
	return c.lines(c.tail(entry.ownOutput(streams), transcript, echo), entry, transcript, prompt)
}

// ownOutput is the text in which the CLI prints its own messages (health.py
// cli_own_output): a merged stream whole; with the streams captured apart only
// its own streams, so the model's answer on stdout is never its verdict.
func (e *cliCatalogEntry) ownOutput(streams cliStreams) string {
	if !streams.apart {
		return streams.output
	}
	var parts []string
	if streams.output != "" && slices.Contains(e.ownStreams, "stdout") {
		parts = append(parts, streams.output)
	}
	if streams.stderr != "" && slices.Contains(e.ownStreams, "stderr") {
		parts = append(parts, streams.stderr)
	}
	return strings.Join(parts, "\n")
}

// cliText is the part of a CLI's output that lines reads: all of it, or the
// tail of a long output (tail) with what comes before it carried over.
type cliText struct {
	text string
	// before: the output before text, when text is the tail of a longer
	// output. A transcript may then be inside a section whose header the tail
	// does not show.
	before string
	// openFence: a markdown fence opened before text is still open.
	openFence bool
	// glog: before has glog lines, so glog entries are skipped in text too;
	// glogFollow: text may start inside a glog entry whose header is too far
	// back to read (its lines up to the entry's end are skipped).
	glog, glogFollow bool
}

// lines returns every line of output the CLI may have written itself
// (health.py _cli_lines). Skipped as diagnostics or task content: glog
// entries, lines inside closed markdown fences, indented lines (code, tool
// results) and tracing lines below ERROR. The indented entries of an anyhow
// "Caused by:" chain and ERROR tracing lines count as marked. With
// entry.jsonReports a JSON object printed over several lines is one line
// (cliJSONReports). With transcript (codex exec), a text with transcript
// structure keeps only the CLI's marked lines outside the sections, plus its
// own error events, which end a section, and the fatal report the text ends
// with; the echo of prompt is skipped whatever it contains. The tail of a
// longer output is a transcript when it or the output's first lines show that
// structure, and it starts inside a section it does not name: up to its first
// section header only codex's own error events count, and no fatal report
// follows that section. What a section longer than the tail ends with may so
// be missed, never task content taken for a refusal.
func (c *cliMessageCatalog) lines(output cliText, entry *cliCatalogEntry, transcript *cliTranscript, prompt string) []cliLine {
	lines := pySplitLines(output.text)
	skipped := map[int]bool{}
	if output.glog || cliHasGlog(output.text) {
		skipped = c.glogEntryLines(lines, output.glogFollow)
	}
	// Markdown code fences belong to the model's answer: what it quotes inside a
	// closed fence is task content. An unclosed (truncated) fence hides nothing.
	// The marks are markdown's, not catalog data: health.py's _FENCE_PREFIXES
	// and cliOpenFence use the same two.
	var fences []int
	if output.openFence {
		fences = append(fences, -1)
	}
	for i, raw := range lines {
		if trimmed := pyLStrip(raw); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fences = append(fences, i)
		}
	}
	for i := 0; i+1 < len(fences); i += 2 {
		for j := max(fences[i], 0); j <= fences[i+1]; j++ {
			skipped[j] = true
		}
	}
	var reports map[int]string
	if entry.jsonReports {
		reports = cliJSONReports(lines, skipped)
	}
	normalized := make([]string, len(lines)) // "" for a skipped line, like health.py's None
	for i, raw := range lines {
		if skipped[i] {
			continue
		}
		if report, ok := reports[i]; ok {
			normalized[i] = normalizeCLIText(truncateRunes(report, 4*cliMessageMaxChars))
		} else {
			normalized[i] = normalizeCLILine(raw)
		}
	}
	section := false
	if transcript != nil {
		if slices.ContainsFunc(normalized, func(line string) bool { return line != "" && transcript.startsSection(line) }) ||
			(output.before != "" && transcript.headStartsSection(output.before)) {
			section = output.before != ""
		} else {
			transcript = nil
		}
	}
	var echoes map[int]int
	fatal := len(lines)
	if transcript != nil {
		echoes = transcript.promptEchoes(lines, normalized, prompt)
		fatal = transcript.fatalReportStart(c, lines, normalized)
	}
	var selected []cliLine
	chain := false
	header := "" // the header of the current section
	skipUntil := 0
	for i, raw := range lines {
		line := normalized[i]
		if i < skipUntil {
			continue
		}
		if i == fatal && section && transcript.fatalAfter.MatchString(header) {
			section = false // the fatal report the text ends with is the CLI's own
		}
		if line == "" {
			chain = false
			continue
		}
		bare := cliBareStart(raw)
		indented := strings.HasPrefix(bare, "\t") || strings.HasPrefix(bare, "  ")
		if chain && indented {
			message := line
			if m := c.causedByEntry.FindStringIndex(line); m != nil {
				message = line[m[1]:]
			}
			selected = append(selected, cliLine{message: message, line: line, marked: true, chain: true})
			continue
		}
		chain = false
		var tracing []int
		if '0' <= line[0] && line[0] <= '9' { // tracing lines start with a timestamp
			tracing = c.tracing.FindStringSubmatchIndex(line)
		}
		errorTracing := tracing != nil && cliSubmatch(line, tracing, c.tracingLevel) == "ERROR"
		if transcript != nil {
			if end, ok := echoes[i]; ok {
				skipUntil, section = end, false
				continue
			}
			if transcript.startsSection(line) {
				section, header = true, line
				continue
			}
			if section {
				// codex prints its events unindented; an indented copy is content.
				message, event := "", false
				if first, _ := utf8.DecodeRuneInString(bare); bare == "" || !pyIsSpace(first) {
					message, event = transcript.ownErrorMessage(line)
				}
				if !event {
					if errorTracing && transcript.tracingTarget.MatchString(cliSubmatch(line, tracing, c.tracingTarget)) {
						selected = append(selected, cliLine{message: line[tracing[1]:], line: line, marked: true})
					}
					continue
				}
				section = false
				selected = append(selected, cliLine{message: message, line: line, marked: true})
				continue
			}
		}
		if indented {
			continue
		}
		if c.causedBy.MatchString(line) {
			chain = true
			continue
		}
		if tracing != nil {
			if errorTracing {
				selected = append(selected, cliLine{message: line[tracing[1]:], line: line, marked: true})
			}
			continue
		}
		if transcript != nil && !c.startsMarked(line, entry.own) {
			continue // codex's own lines outside the sections carry its markers
		}
		selected = append(selected, cliLine{message: line, line: line})
	}
	return selected
}

// finalReport is the error report a CLI ends with: its last line and its last
// marked line, each with the "Caused by:" chain entries that follow it
// (health.py _final_report). The answer, and the tool output it quotes, comes
// before the CLI's verdict.
func (c *cliMessageCatalog) finalReport(selected []cliLine, own *regexp.Regexp) []cliLine {
	if len(selected) == 0 {
		return nil
	}
	last := len(selected) - 1
	for last > 0 && selected[last].chain { // a chain entry: the report starts at its head
		last--
	}
	heads := []int{last}
	marked := -1
	for i, s := range selected {
		if !s.chain && (s.marked || c.startsMarked(s.message, own)) {
			marked = i
		}
	}
	if marked >= 0 && marked != last {
		heads = append(heads, marked)
		slices.Sort(heads)
	}
	var report []cliLine
	for _, head := range heads {
		end := head + 1
		for end < len(selected) && selected[end].chain {
			end++
		}
		report = append(report, selected[head:end]...)
	}
	return report
}

// messageStarts calls yield(start, marked) for every place in message a CLI
// message may start (health.py _message_starts) until yield returns false,
// and reports whether it ran to the end: message itself, then what follows
// each element the CLI prints in front of its own messages (its name, an error
// glyph, a bracketed timestamp or level, an error marker, a gRPC status code),
// at most 8 times; behind an error marker (marked) also what follows the first
// lead-in clause ("Error: turn failed: <message>").
func (c *cliMessageCatalog) messageStarts(message string, marked bool, own *regexp.Regexp, yield func(start string, marked bool) bool) bool {
	rest := message
	for range 8 {
		if rest == "" {
			return true
		}
		if !yield(rest, marked) {
			return false
		}
		var m []int
		if own != nil {
			m = own.FindStringIndex(rest)
		}
		if m == nil {
			if m = c.marker.FindStringSubmatchIndex(rest); m != nil {
				marked = marked || m[2*c.markerGlyph] >= 0 || m[2*c.markerError] >= 0 ||
					(m[2*c.markerBracket] >= 0 && c.markedBracket.MatchString(cliSubmatch(rest, m, c.markerBracket)))
			} else if !marked {
				return true
			} else if m = c.leadIn.FindStringIndex(rest); m == nil {
				return true
			}
		}
		rest = rest[m[1]:]
	}
	return true
}

// startsMarked reports whether an error marker (glyph, "Error:", "[ERROR]"
// ...) leads message (health.py _starts_marked).
func (c *cliMessageCatalog) startsMarked(message string, own *regexp.Regexp) bool {
	marked := false
	c.messageStarts(message, false, own, func(_ string, startMarked bool) bool {
		marked = startMarked
		return !marked
	})
	return marked
}

// glogEntryLines marks the glog diagnostics in lines: each header and its
// continuation (health.py glog_entry_lines, continuation="colon"): a logged
// JSON body while its brackets are open, on lines shaped like JSON, and after
// a header ending in ":" the plain lines up to the next blank, glog or
// top-level line ("Error:", "warning:", "Caused by:"). follow starts the
// lines inside such a continuation.
func (c *cliMessageCatalog) glogEntryLines(lines []string, follow bool) map[int]bool {
	skipped := map[int]bool{}
	depth := 0
	for i, raw := range lines {
		line := pyRStrip(stripCLIANSI(raw))
		stripped := pyStrip(line)
		header := c.glogHeaderIndex(stripped)
		switch {
		case header != nil:
			message := stripped[header[1]:]
			depth, follow = max(0, cliBracketDepth(message)), strings.HasSuffix(message, ":")
			skipped[i] = true
		case depth > 0 && cliJSONBodyLine(stripped):
			depth = max(0, depth+cliBracketDepth(line))
			skipped[i] = true
		case follow && stripped != "" && !c.topLevel.MatchString(normalizeCLIText(stripped)):
			depth = max(0, depth+cliBracketDepth(line))
			skipped[i] = true
		default:
			depth, follow = 0, false
		}
	}
	return skipped
}

// glogHeaderIndex locates the glog header that starts stripped (a line with
// ANSI escapes and surrounding whitespace removed), nil if none.
func (c *cliMessageCatalog) glogHeaderIndex(stripped string) []int {
	if stripped == "" || strings.IndexByte("IWEF", stripped[0]) < 0 {
		return nil
	}
	return c.glogHeader.FindStringIndex(stripped)
}

// setsGlogState reports whether line (raw) sets glogEntryLines' state
// whatever precedes it: a glog header, or a top-level line, which no entry
// continues into.
func (c *cliMessageCatalog) setsGlogState(line string) bool {
	stripped := pyStrip(stripCLIANSI(line))
	return c.glogHeaderIndex(stripped) != nil || (stripped != "" && c.topLevel.MatchString(normalizeCLIText(stripped)))
}

func (t *cliTranscript) startsSection(line string) bool {
	return t.banner.MatchString(line) || t.section.MatchString(line) || t.execResult.MatchString(line)
}

// ownErrorMessage returns the message of a line that is codex's own error
// event ("ERROR: <own error>"). error_event ends in a space, so matching
// own_error on the rest is Python's own_error.match(line, event.end()).
func (t *cliTranscript) ownErrorMessage(line string) (string, bool) {
	event := t.errorEvent.FindStringIndex(line)
	if event == nil || !t.ownError.MatchString(line[event[1]:]) {
		return "", false
	}
	return line[event[1]:], true
}

// promptEchoes maps each prompt echo header followed by the prompt as sent to
// the index after the echo.
func (t *cliTranscript) promptEchoes(lines, normalized []string, prompt string) map[int]int {
	var wanted []string
	for _, line := range pySplitLines(prompt) {
		wanted = append(wanted, pyRStrip(line))
	}
	echoes := map[int]int{}
	if len(wanted) == 0 {
		return echoes
	}
	for i, line := range normalized {
		if line == "" || !t.promptEcho.MatchString(line) || i+1+len(wanted) > len(lines) {
			continue
		}
		echoed := true
		for j, want := range wanted {
			if pyRStrip(stripCLIANSI(lines[i+1+j])) != want {
				echoed = false
				break
			}
		}
		if echoed {
			echoes[i] = i + 1 + len(wanted)
		}
	}
	return echoes
}

// startsSectionRaw is startsSection for a line as the CLI printed it.
func (t *cliTranscript) startsSectionRaw(raw string) bool {
	line := normalizeCLILine(raw)
	return line != "" && t.startsSection(line)
}

// headStartsSection reports whether one of the first lines of output starts
// a section: whether a long output, of which only the tail is read, is a
// transcript (codex exec prints its banner first).
func (t *cliTranscript) headStartsSection(output string) bool {
	head := output[:min(len(output), cliTailHeadBytes)]
	end := 0 // the end of its first complete lines
	for i, lines := 1, 0; i <= len(head) && lines < cliTailHeadLines; i++ {
		if cliTerminatorBefore(head, i) > 0 {
			end, lines = i, lines+1
		}
	}
	for _, raw := range pySplitLines(head[:end]) {
		if line := normalizeCLILine(raw); line != "" && t.startsSection(line) {
			return true
		}
	}
	return false
}

// fatalReportStart is the index of the fatal report the text ends with,
// len(lines) without one (health.py fatal_report_start). A Rust program's
// main prints the error it returns after everything else: "Error: <message>",
// for an anyhow error followed by a blank line, "Caused by:" and the indented
// chain entries; codex exec ends a failed run with it.
func (t *cliTranscript) fatalReportStart(c *cliMessageCatalog, lines, normalized []string) int {
	if t.fatalError == nil || t.fatalAfter == nil {
		return len(lines)
	}
	indented := func(i int) bool {
		bare := cliBareStart(lines[i])
		return strings.HasPrefix(bare, "\t") || strings.HasPrefix(bare, "  ")
	}
	index := len(lines) - 1
	for index >= 0 && normalized[index] == "" {
		index--
	}
	entry := index
	for entry >= 0 && normalized[entry] != "" && indented(entry) {
		entry--
	}
	if entry < index {
		// The text ends with indented lines: only an anyhow chain qualifies.
		if entry < 0 || !c.causedBy.MatchString(normalized[entry]) {
			return len(lines)
		}
		index = entry - 1
		if index < 0 || normalized[index] != "" {
			return len(lines) // anyhow puts a blank line before "Caused by:"
		}
		for index >= 0 && normalized[index] == "" {
			index--
		}
	}
	if index >= 0 && normalized[index] != "" && !indented(index) && t.fatalError.MatchString(normalized[index]) {
		return index
	}
	return len(lines)
}

func cliSubmatch(text string, m []int, group int) string {
	if m[2*group] < 0 {
		return ""
	}
	return text[m[2*group]:m[2*group+1]]
}

// cliMessageMaxChars bounds, in characters, the part of one line that is
// classified (health.py CLI_MESSAGE_MAX_CHARS).
const cliMessageMaxChars = 2000

var (
	cliANSIEscape      = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")
	cliANSIEscapeStart = regexp.MustCompile("^(?:" + cliANSIEscape.String() + ")")
	cliApostrophe      = strings.NewReplacer("’", "'", "‘", "'", "ʼ", "'", "′", "'")
	// health.py _GLOG_ANYWHERE_RE (after its "(?m)^\s*") and
	// _JSON_SCALAR_LINE_RE are str patterns without re.ASCII: Unicode \d.
	cliGlogLineStart  = regexp.MustCompile(`^(?:\x1b\[[0-9;]*m)?[IWEF]\p{Nd}{4} +\p{Nd}{2}:\p{Nd}{2}:\p{Nd}{2}\.`)
	cliJSONScalarLine = regexp.MustCompile(`^(?:-?\p{Nd}[\p{Nd}.eE+-]*|true|false|null),?$`)
)

// cliTailMaxLines and cliTailMaxBytes bound the part of a CLI's output that
// is classified: its last lines, which hold the report the CLI ends with.
// What precedes them is only scanned for the state it leaves (tail), so
// even the largest capture (processjob.MaxOutputBytes of short lines) costs
// little time and memory.
const (
	cliTailMaxLines = 8192
	cliTailMaxBytes = 1 << 20
	// The output a tail is cut from is a transcript when one of its first
	// lines starts a section (headStartsSection).
	cliTailHeadLines = 64
	cliTailHeadBytes = 64 << 10
	// An echo of the prompt is at most four times as long as the prompt and
	// this much more (a CLI may decorate the lines it echoes).
	cliEchoSlackBytes = 64 << 10
)

// tail is the part of output that is classified: all of it, or its last
// lines, at most cliTailMaxLines lines and cliTailMaxBytes bytes but at least
// one line. What comes before the tail is carried over where it decides what
// is task content: a markdown fence it leaves open, a glog entry that runs
// into it and, for a transcript, the section it starts in. The tail starts
// early enough to read the last section header and the last line that sets
// the glog state when they are at most cliTailMaxLines lines and
// cliTailMaxBytes bytes further back, and with prompt (an echo of it in the
// transcript) to hold an echo that reaches into it.
func (c *cliMessageCatalog) tail(output string, transcript *cliTranscript, prompt string) cliText {
	start := cliTailStart(output, cliTailMaxLines, cliTailMaxBytes)
	if start == 0 {
		return cliText{text: output}
	}
	if transcript != nil {
		if header := cliLastLineBefore(output, start, transcript.startsSectionRaw); header >= 0 {
			start = header
		}
	}
	if prompt != "" {
		// An echo that reaches into the tail goes on there with a line of the
		// prompt; it is its header and a line for each line of the prompt, and
		// about as long as the prompt.
		wanted := pySplitLines(prompt)
		first := pyRStrip(stripCLIANSI(cliLineAt(output, start)))
		if slices.ContainsFunc(wanted, func(line string) bool { return pyRStrip(line) == first }) {
			if echo, ok := cliLinesBefore(output, start, len(wanted)+1, 4*len(prompt)+cliEchoSlackBytes); ok {
				start = echo
			}
		}
	}
	glog, follow := cliHasGlog(output[:start]), false
	if glog {
		if anchor := cliLastLineBefore(output, start, c.setsGlogState); anchor >= 0 {
			start = anchor
		} else {
			follow = true // the entry a glog line starts may run into the tail
		}
	}
	if start == 0 {
		return cliText{text: output}
	}
	return cliText{text: output[start:], before: output[:start], openFence: cliOpenFence(output[:start]), glog: glog, glogFollow: follow}
}

// cliLineAt returns the line of text that starts at start (a line start).
func cliLineAt(text string, start int) string {
	for end := start + 1; end <= len(text); end++ {
		if size := cliTerminatorBefore(text, end); size > 0 {
			return text[start : end-size]
		}
	}
	return text[start:]
}

// cliLineBefore returns the line of text that ends where the line starting at
// end starts, and where it starts. end > 0.
func cliLineBefore(text string, end int) (string, int) {
	lineEnd := end - cliTerminatorBefore(text, end)
	start := lineEnd
	for start > 0 && cliTerminatorBefore(text, start) == 0 {
		start--
	}
	return text[start:lineEnd], start
}

// cliTailStart returns where the last lines of text start that together are
// at most maxLines lines and maxBytes bytes long, or the last line when it
// alone is longer; 0 when that is all of text. Lines end as in pySplitLines.
func cliTailStart(text string, maxLines, maxBytes int) int {
	start := len(text)
	end := len(text) - cliTerminatorBefore(text, len(text)) // a final line ending adds no line
	for lines := 0; start > 0; lines++ {
		prev := end
		for prev > 0 && cliTerminatorBefore(text, prev) == 0 {
			prev--
		}
		if lines > 0 && (lines == maxLines || len(text)-prev > maxBytes) {
			return start
		}
		start = prev
		end = start - cliTerminatorBefore(text, start)
	}
	return 0
}

// cliLinesBefore moves start, where a line of text starts, n lines back
// (fewer at the start of text); ok is false when that is more than maxBytes
// back.
func cliLinesBefore(text string, start, n, maxBytes int) (int, bool) {
	floor := start - maxBytes
	for ; n > 0 && start > 0; n-- {
		_, start = cliLineBefore(text, start)
		if start < floor {
			return 0, false
		}
	}
	return start, true
}

// cliLastLineBefore returns where the last line of text before end (a line
// start) starts for which match (given the line) is true, looking back at
// most cliTailMaxLines lines and cliTailMaxBytes bytes; -1 if there is none.
func cliLastLineBefore(text string, end int, match func(line string) bool) int {
	floor := end - cliTailMaxBytes
	for lines := 0; lines < cliTailMaxLines && end > 0; lines++ {
		line, start := cliLineBefore(text, end)
		if start < floor {
			return -1
		}
		if match(line) {
			return start
		}
		end = start
	}
	return -1
}

// cliTerminatorBefore is the length of the line ending (a pySplitLines line
// boundary) that ends text[:i], 0 if none.
func cliTerminatorBefore(text string, i int) int {
	if i == 0 {
		return 0
	}
	switch text[i-1] {
	case '\n':
		if i >= 2 && text[i-2] == '\r' {
			return 2
		}
		return 1
	case '\r':
		if i < len(text) && text[i] == '\n' {
			return 0 // the first half of "\r\n"
		}
		return 1
	case '\v', '\f', 0x1c, 0x1d, 0x1e:
		return 1
	case 0x85: // U+0085
		if i >= 2 && text[i-2] == 0xc2 {
			return 2
		}
	case 0xa8, 0xa9: // U+2028, U+2029
		if i >= 3 && text[i-3] == 0xe2 && text[i-2] == 0x80 {
			return 3
		}
	}
	return 0
}

// cliOpenFence reports whether text leaves a markdown fence open: an odd
// number of its lines start with ``` or ~~~ after whitespace, the fences
// lines pairs. Only the marks are looked at, so the scan stays fast.
func cliOpenFence(text string) bool {
	open := false
	for _, mark := range []string{"```", "~~~"} {
		for i := 0; ; {
			j := strings.Index(text[i:], mark)
			if j < 0 {
				break
			}
			if cliAtLineStart(text, i+j) {
				open = !open
			}
			i += j + len(mark)
		}
	}
	return open
}

// cliAtLineStart reports whether only whitespace precedes text[i] on its line.
func cliAtLineStart(text string, i int) bool {
	for i > 0 && cliTerminatorBefore(text, i) == 0 {
		r, size := utf8.DecodeLastRuneInString(text[:i])
		if !pyIsSpace(r) {
			return false
		}
		i -= size
	}
	return true
}

// cliHasGlog reports whether a glog line is anywhere in text: health.py
// _GLOG_ANYWHERE_RE.search, "(?m)^\s*" followed by a glog header. Only the
// characters a header can start with are visited, so the scan stays fast on
// the longest output.
func cliHasGlog(text string) bool {
	for i := 0; ; i++ {
		next := strings.IndexAny(text[i:], "\x1bIWEF")
		if next < 0 {
			return false
		}
		i += next
		if cliGlogCandidate(text[i:]) && cliAfterLineStart(text, i) && cliGlogLineStart.MatchString(text[i:]) {
			return true
		}
	}
}

// cliGlogCandidate reports whether text may start with a glog header
// (cliGlogLineStart): after an optional color code, I, W, E or F and a byte a
// digit may start with.
func cliGlogCandidate(text string) bool {
	i := 0
	if strings.HasPrefix(text, "\x1b[") {
		for i = 2; i < len(text) && ('0' <= text[i] && text[i] <= '9' || text[i] == ';'); i++ {
		}
		if i == len(text) || text[i] != 'm' {
			return false
		}
		i++
	}
	return i+1 < len(text) && strings.IndexByte("IWEF", text[i]) >= 0 && ('0' <= text[i+1] && text[i+1] <= '9' || text[i+1] >= utf8.RuneSelf)
}

// cliAfterLineStart reports whether only whitespace precedes text[i] since
// the start of text or a "\n": Python's (?m)^\s*.
func cliAfterLineStart(text string, i int) bool {
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:i])
		if r == '\n' {
			return true
		}
		if !pyIsSpace(r) {
			return false
		}
		i -= size
	}
	return true
}

// cliBareStart is the first two characters of stripCLIANSI(line), all that the
// indentation of a line needs, without stripping the whole line.
func cliBareStart(line string) string {
	if strings.IndexByte(line, '\x1b') < 0 {
		return truncateRunes(line, 2)
	}
	var b strings.Builder
	for i, n := 0, 0; i < len(line) && n < 2; n++ {
		for i < len(line) && line[i] == '\x1b' {
			m := cliANSIEscapeStart.FindStringIndex(line[i:])
			if m == nil {
				break
			}
			i += m[1]
		}
		if i == len(line) {
			break
		}
		_, size := utf8.DecodeRuneInString(line[i:])
		b.WriteString(line[i : i+size])
		i += size
	}
	return b.String()
}

// normalizeCLILine returns a line as the catalog patterns read it (health.py
// normalize_cli_line): ANSI stripped, typographic apostrophes as "'",
// whitespace runs collapsed to one space, at most cliMessageMaxChars long.
func normalizeCLILine(line string) string {
	return normalizeCLIText(stripCLIANSI(truncateRunes(line, 4*cliMessageMaxChars)))
}

// normalizeCLIText is health.py _normalize_bare.
func normalizeCLIText(text string) string {
	text = truncateRunes(text, cliMessageMaxChars)
	if cliNormalizedText(text) {
		return text
	}
	text = cliApostrophe.Replace(text)
	var b strings.Builder
	space := false
	for _, r := range text {
		if pyIsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// cliNormalizedText reports whether normalizeCLIText leaves text as it is (no
// typographic apostrophe, no invalid UTF-8, no whitespace but single spaces
// between other characters), which spares most lines an allocation.
func cliNormalizedText(text string) bool {
	space := true // a space here would lead the text
	for _, r := range text {
		switch {
		case r == ' ' && !space:
			space = true
			continue
		case pyIsSpace(r), r == utf8.RuneError, r == '’', r == '‘', r == 'ʼ', r == '′':
			return false
		}
		space = false
	}
	return !space || text == ""
}

func stripCLIANSI(text string) string {
	if strings.IndexByte(text, '\x1b') < 0 {
		return text
	}
	return cliANSIEscape.ReplaceAllString(text, "")
}

// truncateRunes returns at most the first n characters of s, as Python's s[:n].
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// pyIsSpace is Python's str.isspace for one character.
func pyIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func pyStrip(s string) string  { return strings.TrimFunc(s, pyIsSpace) }
func pyLStrip(s string) string { return strings.TrimLeftFunc(s, pyIsSpace) }
func pyRStrip(s string) string { return strings.TrimRightFunc(s, pyIsSpace) }

// pySplitLines is Python's str.splitlines(): lines end at \n, \r, \r\n, \v,
// \f, \x1c, \x1d, \x1e, U+0085, U+2028 and U+2029, and a final line ending
// adds no empty line.
func pySplitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\r':
			lines = append(lines, s[start:i])
			i += size
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
			continue
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i])
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// cliJSONBodyLine is health.py _json_body_line: a line a logged JSON body may
// consist of (blank, bracket- or quote-led, or a bare scalar).
func cliJSONBodyLine(stripped string) bool {
	return stripped == "" || strings.IndexByte(`{}[]",`, stripped[0]) >= 0 || cliJSONScalarLine.MatchString(stripped)
}

// cliJSONReports is health.py _json_report_lines: the JSON objects a CLI
// prints over several lines, each as one line. A report starts on a line that
// does not start with whitespace, ends in "{" or "[" and leaves brackets open
// ("[ERROR] {", "{"); the JSON-shaped lines after it belong to it until its
// brackets close. It returns each report's first index -> its lines stripped
// and joined by one space, and adds the report's other lines to skipped.
func cliJSONReports(lines []string, skipped map[int]bool) map[int]string {
	joined := map[int]string{}
	for index := 0; index < len(lines); {
		head := pyRStrip(stripCLIANSI(lines[index]))
		depth := 0
		if first, _ := utf8.DecodeRuneInString(head); !skipped[index] && !pyIsSpace(first) &&
			(strings.HasSuffix(head, "{") || strings.HasSuffix(head, "[")) {
			depth = cliBracketDepth(head)
		}
		end := index + 1
		for depth > 0 && end < len(lines) && !skipped[end] {
			stripped := pyStrip(stripCLIANSI(lines[end]))
			if !cliJSONBodyLine(stripped) {
				break
			}
			depth += cliBracketDepth(stripped)
			end++
		}
		if end > index+1 {
			parts := []string{head}
			for i := index + 1; i < end; i++ {
				parts = append(parts, pyStrip(stripCLIANSI(lines[i])))
				skipped[i] = true
			}
			joined[index] = strings.Join(parts, " ")
		}
		index = end
	}
	return joined
}

// cliBracketDepth is the change in JSON bracket depth over text.
func cliBracketDepth(text string) int {
	depth, inString, escaped := 0, false, false
	for _, r := range text {
		switch {
		case inString && escaped:
			escaped = false
		case inString && r == '\\':
			escaped = true
		case inString:
			inString = r != '"'
		case r == '"':
			inString = true
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
		}
	}
	return depth
}

// cliCatalogRegexp compiles a catalog pattern to match what health.py's
// cli_catalog_regex(pattern).match matches (Python re, re.IGNORECASE |
// re.ASCII): anchored at the start of the text, case-insensitive for ASCII
// letters only. Go's (?i) would also fold the Kelvin sign into k, the long s
// into s and Greek mu into µ, so asciiFoldPattern spells the cases out.
func cliCatalogRegexp(pattern string) (*regexp.Regexp, error) {
	folded, err := asciiFoldPattern(pattern)
	if err != nil {
		return nil, err
	}
	return regexp.Compile(`^(?:` + folded + `)`)
}

// asciiFoldPattern rewrites a pattern of the catalog's dialect (the syntax
// Python's re and RE2 share) for Go's regexp without (?i): each ASCII letter
// outside a (?-i:...) group matches both of its cases, in character classes
// too, and \s and \S are Python's ASCII classes (Go's \s lacks \v). Syntax
// whose meaning differs between the two engines is rejected.
func asciiFoldPattern(pattern string) (string, error) {
	var out strings.Builder
	fold := true
	var groups []bool // the fold state each open group restores
	for i := 0; i < len(pattern); {
		r, size := utf8.DecodeRuneInString(pattern[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			return "", fmt.Errorf("invalid UTF-8 at offset %d", i)
		case r == '\\':
			esc, err := foldEscape(pattern[i:], false)
			if err != nil {
				return "", err
			}
			out.WriteString(esc.text)
			size = esc.size
		case r == '[':
			class, n, err := foldClass(pattern[i:], fold)
			if err != nil {
				return "", err
			}
			out.WriteString(class)
			size = n
		case r == '(' && strings.HasPrefix(pattern[i:], "(?P<"):
			end := strings.IndexByte(pattern[i:], '>')
			if end < 0 {
				return "", fmt.Errorf("unterminated group name at offset %d", i)
			}
			groups = append(groups, fold)
			out.WriteString(pattern[i : i+end+1])
			size = end + 1
		case r == '(' && strings.HasPrefix(pattern[i:], "(?"):
			// (?:...), (?flags:...) or (?flags); only i, m and s mean the same
			// in both engines, and i is spelled out instead.
			end := strings.IndexAny(pattern[i+2:], ":)")
			if end < 0 {
				return "", fmt.Errorf("unterminated group at offset %d", i)
			}
			flags, scoped := pattern[i+2:i+2+end], pattern[i+2+end] == ':'
			on, off, _ := strings.Cut(flags, "-")
			if strings.Trim(on+off, "ims") != "" || strings.Count(flags, "-") > 1 || (!scoped && on+off == "") {
				return "", fmt.Errorf("unsupported group %q at offset %d", pattern[i:i+3+end], i)
			}
			inner := fold
			if strings.Contains(on, "i") {
				inner = true
			}
			if strings.Contains(off, "i") {
				inner = false
			}
			kept := strings.ReplaceAll(on, "i", "")
			if off = strings.ReplaceAll(off, "i", ""); off != "" {
				kept += "-" + off
			}
			if scoped {
				groups = append(groups, fold)
				out.WriteString("(?" + kept + ":")
			} else if kept != "" {
				out.WriteString("(?" + kept + ")")
			}
			fold = inner
			size = 3 + end
		case r == '(':
			groups = append(groups, fold)
			out.WriteByte('(')
		case r == ')':
			if len(groups) == 0 {
				return "", fmt.Errorf("unbalanced ) at offset %d", i)
			}
			fold = groups[len(groups)-1]
			groups = groups[:len(groups)-1]
			out.WriteByte(')')
		case fold && asciiLetter(r):
			out.WriteString("[" + string(unicode.ToLower(r)) + string(unicode.ToUpper(r)) + "]")
		default:
			out.WriteString(pattern[i : i+size])
		}
		i += size
	}
	if len(groups) != 0 {
		return "", errors.New("unbalanced (")
	}
	return out.String(), nil
}

// foldedEscape is an escape sequence of a pattern rewritten by foldEscape: its
// RE2 text, the pattern bytes it spans and, for an escape of one character,
// that character (-1 otherwise).
type foldedEscape struct {
	text string
	size int
	char rune
}

// foldEscape rewrites the escape sequence at the start of s.
func foldEscape(s string, inClass bool) (foldedEscape, error) {
	if len(s) < 2 {
		return foldedEscape{}, errors.New("trailing backslash")
	}
	c := s[1]
	switch {
	case c == 's' && inClass:
		return foldedEscape{text: `\t\n\v\f\r `, size: 2, char: -1}, nil
	case c == 's':
		return foldedEscape{text: `[\t\n\v\f\r ]`, size: 2, char: -1}, nil
	case c == 'S' && !inClass:
		return foldedEscape{text: `[^\t\n\v\f\r ]`, size: 2, char: -1}, nil
	case c == 'd' || c == 'D' || c == 'w' || c == 'W' || (!inClass && (c == 'b' || c == 'B')):
		return foldedEscape{text: s[:2], size: 2, char: -1}, nil
	case strings.IndexByte("tnrfva", c) >= 0:
		return foldedEscape{text: s[:2], size: 2, char: rune("\t\n\r\f\v\a"[strings.IndexByte("tnrfva", c)])}, nil
	case c < utf8.RuneSelf && !asciiLetter(rune(c)) && (c < '0' || c > '9'):
		return foldedEscape{text: s[:2], size: 2, char: rune(c)}, nil // escaped punctuation is itself
	}
	return foldedEscape{}, fmt.Errorf("unsupported escape %q", s[:2])
}

// foldClass rewrites the character class at the start of s and returns it with
// the pattern bytes it spans. With fold, the other case of every ASCII letter
// of the class joins it: Python's re compares a class and the character it
// tests both lower-cased, so a negated class excludes both cases too.
func foldClass(s string, fold bool) (string, int, error) {
	var out strings.Builder
	out.WriteByte('[')
	i := 1
	if i < len(s) && s[i] == '^' {
		out.WriteByte('^')
		i++
	}
	// member reads one character of the class (char >= 0) or a class escape.
	member := func() (foldedEscape, error) {
		if i >= len(s) {
			return foldedEscape{}, errors.New("unterminated character class")
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			return foldedEscape{}, errors.New("invalid UTF-8 in a character class")
		case r == '[':
			return foldedEscape{}, errors.New("[ in a character class")
		case r == '\\':
			esc, err := foldEscape(s[i:], true)
			i += esc.size
			return esc, err
		}
		i += size
		return foldedEscape{size: size, char: r}, nil
	}
	var ranges [][2]rune
	for first := true; ; first = false {
		if i >= len(s) {
			return "", 0, errors.New("unterminated character class")
		}
		if s[i] == ']' && !first {
			i++
			break
		}
		lo, err := member()
		if err != nil {
			return "", 0, err
		}
		isRange := i+1 < len(s) && s[i] == '-' && s[i+1] != ']'
		if lo.char < 0 {
			if isRange {
				return "", 0, errors.New("range from a class escape")
			}
			out.WriteString(lo.text)
			continue
		}
		hi := lo
		if isRange {
			i++
			if hi, err = member(); err != nil {
				return "", 0, err
			}
			if hi.char < lo.char {
				return "", 0, errors.New("invalid character class range")
			}
		}
		ranges = append(ranges, [2]rune{lo.char, hi.char})
		out.WriteString(classChar(lo.char))
		if hi.char != lo.char {
			out.WriteString("-" + classChar(hi.char))
		}
	}
	if fold {
		for _, r := range ranges {
			if lo, hi := max(r[0], 'a'), min(r[1], 'z'); lo <= hi {
				out.WriteString(classChar(lo-'a'+'A') + "-" + classChar(hi-'a'+'A'))
			}
			if lo, hi := max(r[0], 'A'), min(r[1], 'Z'); lo <= hi {
				out.WriteString(classChar(lo-'A'+'a') + "-" + classChar(hi-'A'+'a'))
			}
		}
	}
	out.WriteByte(']')
	return out.String(), i, nil
}

// classChar writes r as a member of an RE2 character class.
func classChar(r rune) string {
	switch {
	case r < ' ' || r == 0x7f:
		return fmt.Sprintf(`\x{%x}`, r)
	case strings.ContainsRune(`\]-^[`, r):
		return `\` + string(r)
	}
	return string(r)
}

func asciiLetter(r rune) bool { return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' }

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

// applyGeminiProxyPolicy bypasses Google hosts only on explicit opt-in.
func applyGeminiProxyPolicy(env []string) []string {
	return proxyconfig.Gemini(env)
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
