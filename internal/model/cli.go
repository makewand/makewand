package model

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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/makewand/makewand/internal/config"
)

// CLIProvider wraps a subscription CLI tool (claude, gemini, codex) as a Provider.
// It calls the CLI in non-interactive/print mode, which uses the user's subscription.
type CLIProvider struct {
	name     string // "claude-cli", "gemini-cli", "codex-cli"
	binPath  string // absolute path to the binary
	provider string // display name: "claude", "gemini", "codex"
	buildCmd func(ctx context.Context, prompt string) *exec.Cmd
	checkCmd func(ctx context.Context) *exec.Cmd

	// jsonOutput indicates the CLI is invoked with a JSON output flag.
	// When true, chatAttempt tries parseJSONResponse before falling back to text.
	jsonOutput        bool
	parseJSONResponse func(raw []byte) (content string, usage *Usage, err error)

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
	p.checkCmd = func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binPath, "--version")
		cmd.Env = applyGeminiProxyPolicy(cmd.Environ())
		return cmd
	}
	return p
}

// --- Codex CLI ---

// NewCodexCLI creates a provider that uses `codex exec` (Codex CLI).
// Task-aware: uses `codex review --uncommitted` for review tasks,
// `codex exec --json` for code/analysis tasks (provides structured usage data).
func NewCodexCLI(binPath string) *CLIProvider {
	p := &CLIProvider{
		name:              "codex-cli",
		binPath:           binPath,
		provider:          "codex",
		jsonOutput:        true,
		parseJSONResponse: parseCodexCLIJSONL,
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		// Use dedicated review subcommand for review tasks.
		if task, ok := TaskFromContext(ctx); ok && task == TaskReview {
			return exec.CommandContext(ctx, binPath, "review", "--uncommitted")
		}
		// Default: exec with JSON output for structured usage data.
		return exec.CommandContext(ctx, binPath, "exec", "--json", "--skip-git-repo-check", prompt)
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
		if promptMode == config.CustomPromptModeStdin {
			cmd := exec.CommandContext(ctx, command, templateArgs...)
			cmd.Stdin = strings.NewReader(prompt)
			return cmd
		}

		finalArgs := make([]string, 0, len(templateArgs)+1)
		if promptMode == config.CustomPromptModeArg {
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
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return false
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err == nil
	case <-ctx.Done():
		killCLIProcess(cmd)
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && c.softPassProbeTimeout() {
			// Some subscription CLIs can stall on --version probes when network/auth is slow.
			// Treat timeout as "unknown but likely available" and let request-time retries decide.
			return true
		}
		return false
	}
}

func (c *CLIProvider) softPassProbeTimeout() bool {
	switch c.provider {
	case "gemini", "claude", "codex":
		return true
	default:
		return false
	}
}

func (c *CLIProvider) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	var prompt string
	if c.systemFlag != "" && system != "" {
		// Pass system prompt via dedicated CLI flag (proper system role).
		prompt = buildCLIPrompt(messages, "")
		ctx = ContextWithSystem(ctx, system)
	} else {
		prompt = buildCLIPrompt(messages, system)
	}

	ctx, cancel := context.WithTimeout(ctx, cliRequestTimeout)
	defer cancel()

	attempts := 0
	for {
		attempts++
		content, jsonUsage, err := c.chatAttempt(ctx, prompt)
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
				return "", Usage{}, fmt.Errorf("%s (after %d attempts)", err.Error(), attempts)
			}
			return "", Usage{}, err
		}

		delay := cliRetryDelay(attempts)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if attempts > 1 {
				return "", Usage{}, fmt.Errorf("%s (after %d attempts)", err.Error(), attempts)
			}
			return "", Usage{}, err
		case <-timer.C:
		}
	}
}

func (c *CLIProvider) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	var prompt string
	if c.systemFlag != "" && system != "" {
		prompt = buildCLIPrompt(messages, "")
		ctx = ContextWithSystem(ctx, system)
	} else {
		prompt = buildCLIPrompt(messages, system)
	}

	ctx, cancel := context.WithTimeout(ctx, cliStreamTimeout)

	cmd := c.buildCmd(ctx, prompt)
	setCLIProcessGroup(cmd)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, newProviderError(c.provider, "CLI pipe", ErrorKindConfig, false, 0, err.Error(), err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, newProviderError(c.provider, "CLI stderr pipe", ErrorKindConfig, false, 0, err.Error(), err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, newProviderError(c.provider, "CLI start", ErrorKindConfig, false, 0, err.Error(), err)
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer cancel()
		defer close(ch)
		start := time.Now()

		var stderr bytes.Buffer
		stderrDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(&stderr, stderrPipe)
			close(stderrDone)
		}()

		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	streamLoop:
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				killCLIProcess(cmd)
				break streamLoop
			default:
			}
			line := stripANSI(scanner.Text())
			if line != "" {
				select {
				case ch <- StreamChunk{Content: line + "\n"}:
				case <-ctx.Done():
					killCLIProcess(cmd)
					break streamLoop
				}
			}
		}

		scanErr := scanner.Err()

		<-stderrDone
		waitErr := cmd.Wait()
		duration := time.Since(start)

		if ctxErr := ctx.Err(); ctxErr != nil {
			if shouldSurfaceContextError(ctxErr) {
				ch <- StreamChunk{Error: formatCLIContextError(c.provider, ctxErr, duration)}
			}
			return
		}

		if scanErr != nil {
			ch <- StreamChunk{Error: newProviderError(c.provider, "CLI stream", ErrorKindProvider, false, 0, scanErr.Error(), scanErr)}
			return
		}

		if waitErr != nil {
			ch <- StreamChunk{Error: formatCLIExecutionError(c.provider, stderr.String(), waitErr, nil, duration)}
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
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		Cost:         resp.TotalCostUSD,
		Provider:     "claude",
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
		}
		break // Take the first (usually only) model entry.
	}

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
	usage.Model = modelName

	return content, usage, nil
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
		Type            string `json:"type"`
		Text            string `json:"text"`             // agent_message text
		AggregatedOutput string `json:"aggregated_output"` // command_execution output
		Status          string `json:"status"`
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
					InputTokens:  event.Usage.InputTokens,
					OutputTokens: event.Usage.OutputTokens,
					Cost:         0, // Codex CLI uses subscription; no per-token cost.
					Provider:     "codex",
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

func (c *CLIProvider) chatAttempt(ctx context.Context, prompt string) (string, *Usage, error) {
	cmd := c.buildCmd(ctx, prompt)
	setCLIProcessGroup(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", nil, newProviderError(c.provider, "CLI start", ErrorKindConfig, false, 0, err.Error(), err)
	}

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
	if runErr != nil {
		return "", nil, formatCLIExecutionError(c.provider, stderr.String(), runErr, ctx.Err(), duration)
	}

	raw := stdout.Bytes()

	// Try structured JSON parsing when the CLI was invoked with a JSON output flag.
	if c.jsonOutput && c.parseJSONResponse != nil {
		content, usage, err := c.parseJSONResponse(raw)
		if err == nil && content != "" {
			if reject, reason := shouldRejectCLIOutput(prompt, content); reject {
				return "", nil, newProviderError(c.provider, "CLI output", ErrorKindConfig, false, 0, reason, nil)
			}
			return content, usage, nil
		}
		// JSON parse failed — fall back to text mode below.
	}

	content := strings.TrimSpace(string(raw))
	content = stripANSI(content)
	if reject, reason := shouldRejectCLIOutput(prompt, content); reject {
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
		strings.Contains(lower, "cannot write files in this environment")
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
		if strings.Contains(l, "=>") || strings.Contains(l, ";") {
			return true
		}
	}
	return false
}

func isTransientCLIError(err error) bool {
	if err == nil {
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

func formatCLIExecutionError(provider, stderr string, runErr error, ctxErr error, duration time.Duration) error {
	if ctxErr != nil {
		return formatCLIContextError(provider, ctxErr, duration)
	}

	errMsg := strings.TrimSpace(stderr)
	if errMsg == "" {
		errMsg = runErr.Error()
	}
	base := newProviderError(provider, "CLI", ErrorKindProvider, false, 0, errMsg, runErr)
	if looksTransient(errMsg) {
		return &TransientCLIError{Err: newProviderError(provider, "CLI", ErrorKindNetwork, true, 0, errMsg, runErr)}
	}
	return base
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
				for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
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
