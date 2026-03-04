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
)

// CLIProvider wraps a subscription CLI tool (claude, gemini, codex) as a Provider.
// It calls the CLI in non-interactive/print mode, which uses the user's subscription.
type CLIProvider struct {
	name     string // "claude-cli", "gemini-cli", "codex-cli"
	binPath  string // absolute path to the binary
	provider string // display name: "claude", "gemini", "codex"
	buildCmd func(ctx context.Context, prompt string) *exec.Cmd
	checkCmd func(ctx context.Context) *exec.Cmd

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
		name:     "claude-cli",
		binPath:  binPath,
		provider: "claude",
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binPath, "-p", prompt)
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
		name:     "gemini-cli",
		binPath:  binPath,
		provider: "gemini",
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binPath, "-p", prompt, "--sandbox", "false")
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
func NewCodexCLI(binPath string) *CLIProvider {
	p := &CLIProvider{
		name:     "codex-cli",
		binPath:  binPath,
		provider: "codex",
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		return exec.CommandContext(ctx, binPath, "exec", "--skip-git-repo-check", prompt)
	}
	p.checkCmd = func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, binPath, "--version")
	}
	return p
}

// NewCommandCLI creates a provider backed by an arbitrary CLI command.
// Prompt injection behavior:
//   - if any argument contains "{{prompt}}", that token is replaced in-place
//   - otherwise, the prompt is appended as the final argument
func NewCommandCLI(providerName, command string, args []string) *CLIProvider {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	command = strings.TrimSpace(command)
	templateArgs := append([]string(nil), args...)

	p := &CLIProvider{
		name:     providerName + "-custom-cli",
		binPath:  command,
		provider: providerName,
	}
	p.buildCmd = func(ctx context.Context, prompt string) *exec.Cmd {
		finalArgs := make([]string, 0, len(templateArgs)+1)
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
		return false
	}
}

func (c *CLIProvider) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	prompt := buildCLIPrompt(messages, system)

	ctx, cancel := context.WithTimeout(ctx, cliRequestTimeout)
	defer cancel()

	attempts := 0
	for {
		attempts++
		content, err := c.chatAttempt(ctx, prompt)
		if err == nil {
			usage := Usage{
				InputTokens:  estimateTokens(prompt),
				OutputTokens: estimateTokens(content),
				Cost:         0, // Subscription — no per-token cost
				Model:        c.provider + "-cli",
				Provider:     c.provider,
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
	prompt := buildCLIPrompt(messages, system)

	ctx, cancel := context.WithTimeout(ctx, cliStreamTimeout)

	cmd := c.buildCmd(ctx, prompt)
	setCLIProcessGroup(cmd)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s CLI pipe: %w", c.provider, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s CLI stderr pipe: %w", c.provider, err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%s CLI start: %w", c.provider, err)
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
			ch <- StreamChunk{Error: fmt.Errorf("%s CLI stream error: %w", c.provider, scanErr)}
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

// --- Helpers ---

func shouldSurfaceContextError(ctxErr error) bool {
	return errors.Is(ctxErr, context.DeadlineExceeded)
}

func (c *CLIProvider) chatAttempt(ctx context.Context, prompt string) (string, error) {
	cmd := c.buildCmd(ctx, prompt)
	setCLIProcessGroup(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%s CLI start error: %w", c.provider, err)
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
		return "", formatCLIExecutionError(c.provider, stderr.String(), runErr, ctx.Err(), duration)
	}

	content := strings.TrimSpace(stdout.String())
	return stripANSI(content), nil
}

func isTransientCLIError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
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
		return fmt.Errorf("%s CLI timeout after %s", provider, rounded)
	case errors.Is(ctxErr, context.Canceled):
		return fmt.Errorf("%s CLI canceled after %s", provider, rounded)
	default:
		return fmt.Errorf("%s CLI context error after %s: %v", provider, rounded, ctxErr)
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
	return fmt.Errorf("%s CLI error: %s", provider, errMsg)
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

// estimateTokens provides a rough token count (~4 chars per token).
func estimateTokens(text string) int {
	return len(text) / 4
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
