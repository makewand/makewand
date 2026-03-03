package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ExecResult holds the result of a command execution.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// ExecPlan describes one command candidate detected from project files.
type ExecPlan struct {
	Kind     string // "deps" or "tests"
	Detector string // which file triggered this plan
	Command  string
	Args     []string
}

// DisplayCommand renders a shell-like command summary for user confirmation.
func (p ExecPlan) DisplayCommand() string {
	var parts []string
	parts = append(parts, quoteArg(p.Command))
	for _, arg := range p.Args {
		parts = append(parts, quoteArg(arg))
	}
	return strings.Join(parts, " ")
}

type execPolicy struct {
	allowAnyCommand bool
	stripSensitive  bool
	allowedCommands map[string]struct{}
}

var restrictedAllowedCommands = map[string]struct{}{
	"npm":     {},
	"pnpm":    {},
	"yarn":    {},
	"pip":     {},
	"pip3":    {},
	"python":  {},
	"python3": {},
	"go":      {},
	"cargo":   {},
	"pytest":  {},
}

var sensitiveEnvExact = map[string]struct{}{
	"ANTHROPIC_API_KEY":     {},
	"GEMINI_API_KEY":        {},
	"OPENAI_API_KEY":        {},
	"AWS_ACCESS_KEY_ID":     {},
	"AWS_SECRET_ACCESS_KEY": {},
	"AWS_SESSION_TOKEN":     {},
	"GITHUB_TOKEN":          {},
	"GH_TOKEN":              {},
	"NPM_TOKEN":             {},
}

var sensitiveEnvSuffixes = []string{
	"_API_KEY",
	"_TOKEN",
	"_SECRET",
	"_PASSWORD",
}

func trustedExecPolicy() execPolicy {
	return execPolicy{
		allowAnyCommand: true,
		stripSensitive:  false,
	}
}

func restrictedExecPolicy() execPolicy {
	return execPolicy{
		allowAnyCommand: false,
		stripSensitive:  true,
		allowedCommands: restrictedAllowedCommands,
	}
}

// Exec runs a trusted command in the project directory with a timeout.
// Used by internal engine flows like git operations.
func (p *Project) Exec(ctx context.Context, command string, args ...string) (*ExecResult, error) {
	return p.execWithPolicy(ctx, command, args, trustedExecPolicy())
}

// ExecRestricted runs a restricted command in the project directory.
// Command name must be allowlisted and sensitive environment variables are stripped.
func (p *Project) ExecRestricted(ctx context.Context, command string, args ...string) (*ExecResult, error) {
	return p.execWithPolicy(ctx, command, args, restrictedExecPolicy())
}

// RunRestrictedPlan executes a detected restricted plan.
func (p *Project) RunRestrictedPlan(ctx context.Context, plan ExecPlan) (*ExecResult, error) {
	return p.ExecRestricted(ctx, plan.Command, plan.Args...)
}

// DetectInstallPlan detects the dependency install command for the current project.
func (p *Project) DetectInstallPlan() (*ExecPlan, error) {
	candidates := []struct {
		file    string
		command string
		args    []string
	}{
		{"package.json", "npm", []string{"install", "--ignore-scripts"}},
		{"requirements.txt", "pip", []string{"install", "--user", "-r", "requirements.txt"}},
		{"pyproject.toml", "pip", []string{"install", "--user", "-e", "."}},
		{"go.mod", "go", []string{"mod", "tidy"}},
		{"Cargo.toml", "cargo", []string{"build"}},
	}

	for _, c := range candidates {
		content, err := p.ReadFile(c.file)
		if err == nil && strings.TrimSpace(content) != "" {
			return &ExecPlan{
				Kind:     "deps",
				Detector: c.file,
				Command:  c.command,
				Args:     append([]string(nil), c.args...),
			}, nil
		}
	}
	return nil, nil
}

// DetectTestPlan detects the test execution command for the current project.
func (p *Project) DetectTestPlan() (*ExecPlan, error) {
	if content, err := p.ReadFile("package.json"); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal([]byte(content), &pkg) == nil {
			if _, ok := pkg.Scripts["test"]; ok {
				return &ExecPlan{
					Kind:     "tests",
					Detector: "package.json",
					Command:  "npm",
					Args:     []string{"test"},
				}, nil
			}
		}
	}
	if _, err := p.ReadFile("pytest.ini"); err == nil {
		return &ExecPlan{
			Kind:     "tests",
			Detector: "pytest.ini",
			Command:  "pytest",
			Args:     nil,
		}, nil
	}
	if _, err := p.ReadFile("go.mod"); err == nil {
		return &ExecPlan{
			Kind:     "tests",
			Detector: "go.mod",
			Command:  "go",
			Args:     []string{"test", "./..."},
		}, nil
	}
	if _, err := p.ReadFile("Cargo.toml"); err == nil {
		return &ExecPlan{
			Kind:     "tests",
			Detector: "Cargo.toml",
			Command:  "cargo",
			Args:     []string{"test"},
		}, nil
	}
	return nil, nil
}

// InstallDeps detects the project type and installs dependencies.
func (p *Project) InstallDeps(ctx context.Context) (*ExecResult, error) {
	plan, err := p.DetectInstallPlan()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &ExecResult{Stdout: "No package manager detected"}, nil
	}
	return p.RunRestrictedPlan(ctx, *plan)
}

// RunTests detects and runs the project's test suite.
func (p *Project) RunTests(ctx context.Context) (*ExecResult, error) {
	plan, err := p.DetectTestPlan()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &ExecResult{Stdout: "No test framework detected"}, nil
	}
	return p.RunRestrictedPlan(ctx, *plan)
}

// limitedWriter wraps a bytes.Buffer but silently discards data beyond the limit.
type limitedWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil // discard
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}

func (w *limitedWriter) String() string { return w.buf.String() }

const maxOutputBytes = 10 << 20 // 10 MB

func (p *Project) execWithPolicy(ctx context.Context, command string, args []string, policy execPolicy) (*ExecResult, error) {
	if err := validateCommandName(command); err != nil {
		return nil, err
	}
	if !policy.allowAnyCommand {
		if _, ok := policy.allowedCommands[command]; !ok {
			return nil, fmt.Errorf("command %q is blocked by execution policy", command)
		}
	}

	projectDir, err := p.execWorkingDir()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.Command(command, args...)
	cmd.Dir = projectDir
	if policy.stripSensitive {
		cmd.Env = sanitizeExecEnv(cmd.Environ())
	}
	setProcessGroup(cmd)

	stdout := &limitedWriter{limit: maxOutputBytes}
	stderr := &limitedWriter{limit: maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %s: %w", command, err)
	}

	// Kill the entire process group when the context deadline is exceeded.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				killProcessGroup(cmd)
			}
		case <-watchDone:
		}
	}()

	err = cmd.Wait()
	close(watchDone)
	duration := time.Since(start)

	result := &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("exec %s: %w", command, err)
		}
	}

	return result, nil
}

func (p *Project) execWorkingDir() (string, error) {
	if p == nil || p.Path == "" {
		return "", fmt.Errorf("project path is empty")
	}
	abs, err := filepath.Abs(p.Path)
	if err != nil {
		return "", fmt.Errorf("resolve project path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("project path not accessible: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project path is not a directory: %s", abs)
	}
	return abs, nil
}

func validateCommandName(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("empty command")
	}
	if strings.Contains(command, "/") || strings.Contains(command, "\\") {
		return fmt.Errorf("command must be a plain executable name: %q", command)
	}
	return nil
}

func sanitizeExecEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		key := entry[:eq]
		if isSensitiveEnv(key) {
			continue
		}
		out = append(out, entry)
	}
	if !hasEnvKey(out, "NO_COLOR") {
		out = append(out, "NO_COLOR=1")
	}
	if !hasEnvKey(out, "MAKEWAND_SANDBOX") {
		out = append(out, "MAKEWAND_SANDBOX=1")
	}
	return out
}

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func isSensitiveEnv(key string) bool {
	if _, ok := sensitiveEnvExact[key]; ok {
		return true
	}
	return slices.ContainsFunc(sensitiveEnvSuffixes, func(s string) bool { return strings.HasSuffix(key, s) })
}

func quoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.ContainsAny(arg, " \t\n\"'`$&|;<>()[]{}*?!") {
		return strconv.Quote(arg)
	}
	return arg
}
