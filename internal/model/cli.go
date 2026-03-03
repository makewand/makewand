package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	mu            sync.Mutex
	cachedAvail   bool
	cachedAvailAt time.Time
}

const cliAvailCacheTTL = 60 * time.Second

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
		// Ensure Google API calls bypass local proxy
		cmd.Env = ensureNoProxy(cmd.Environ())
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

	_, err := exec.LookPath(c.binPath)
	c.cachedAvail = err == nil
	c.cachedAvailAt = time.Now()
	return c.cachedAvail
}

func (c *CLIProvider) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	prompt := buildCLIPrompt(messages, system)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := c.buildCmd(ctx, prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", Usage{}, fmt.Errorf("%s CLI error: %s", c.provider, errMsg)
	}

	content := strings.TrimSpace(stdout.String())

	// Strip ANSI escape sequences from CLI output
	content = stripANSI(content)

	usage := Usage{
		InputTokens:  estimateTokens(prompt),
		OutputTokens: estimateTokens(content),
		Cost:         0, // Subscription — no per-token cost
		Model:        c.provider + "-cli",
		Provider:     c.provider,
	}

	_ = duration // available for future metrics
	return content, usage, nil
}

func (c *CLIProvider) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	prompt := buildCLIPrompt(messages, system)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)

	cmd := c.buildCmd(ctx, prompt)
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

		var stderr bytes.Buffer
		stderrDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(&stderr, stderrPipe)
			close(stderrDone)
		}()

		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line := stripANSI(scanner.Text())
			if line != "" {
				select {
				case ch <- StreamChunk{Content: line + "\n"}:
				case <-ctx.Done():
					return
				}
			}
		}

		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			ch <- StreamChunk{Error: fmt.Errorf("%s CLI stream error: %w", c.provider, err)}
			return
		}

		<-stderrDone
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			errMsg := strings.TrimSpace(stderr.String())
			if errMsg == "" {
				errMsg = err.Error()
			}
			ch <- StreamChunk{Error: fmt.Errorf("%s CLI error: %s", c.provider, errMsg)}
			return
		}

		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

// --- Helpers ---

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

// ensureNoProxy ensures NO_PROXY includes Google API hosts, preventing
// proxy-related TLS failures when a local HTTP proxy is configured.
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
