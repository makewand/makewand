package engine

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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

// Exec runs a command in the project directory with a timeout.
func (p *Project) Exec(ctx context.Context, command string, args ...string) (*ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = p.Path

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	result := &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("exec %s: %w", command, err)
		}
	}

	return result, nil
}

// ExecShell runs a shell command string in the project directory.
func (p *Project) ExecShell(ctx context.Context, command string) (*ExecResult, error) {
	return p.Exec(ctx, "sh", "-c", command)
}

// InstallDeps detects the project type and installs dependencies.
func (p *Project) InstallDeps(ctx context.Context) (*ExecResult, error) {
	// Detect package manager
	managers := []struct {
		file    string
		command string
		args    []string
	}{
		{"package.json", "npm", []string{"install"}},
		{"requirements.txt", "pip", []string{"install", "-r", "requirements.txt"}},
		{"pyproject.toml", "pip", []string{"install", "-e", "."}},
		{"go.mod", "go", []string{"mod", "tidy"}},
		{"Cargo.toml", "cargo", []string{"build"}},
	}

	for _, m := range managers {
		content, err := p.ReadFile(m.file)
		if err == nil && content != "" {
			return p.Exec(ctx, m.command, m.args...)
		}
	}

	return &ExecResult{Stdout: "No package manager detected"}, nil
}

// RunTests detects and runs the project's test suite.
func (p *Project) RunTests(ctx context.Context) (*ExecResult, error) {
	// Detect test framework
	if content, err := p.ReadFile("package.json"); err == nil {
		if strings.Contains(content, `"test"`) {
			return p.Exec(ctx, "npm", "test")
		}
	}
	if _, err := p.ReadFile("pytest.ini"); err == nil {
		return p.Exec(ctx, "pytest")
	}
	if _, err := p.ReadFile("go.mod"); err == nil {
		return p.Exec(ctx, "go", "test", "./...")
	}

	return &ExecResult{Stdout: "No test framework detected"}, nil
}
