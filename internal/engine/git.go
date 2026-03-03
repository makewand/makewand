package engine

import (
	"context"
	"fmt"
	"strings"
)

// GitInit initializes a git repository in the project directory.
func (p *Project) GitInit(ctx context.Context) error {
	result, err := p.Exec(ctx, "git", "init")
	if err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("git init failed: %s", result.Stderr)
	}

	gitignore := `node_modules/
__pycache__/
.venv/
venv/
*.pyc
.DS_Store
.env
dist/
build/
*.log
credentials.json
service-account*.json
.npmrc
*.pem
*.key
`
	if err := p.WriteFile(".gitignore", gitignore); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}

	return nil
}

// GitCommit stages all changes and creates a commit.
func (p *Project) GitCommit(ctx context.Context, message string) error {
	addResult, err := p.Exec(ctx, "git", "add", "-A")
	if err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if addResult.ExitCode != 0 {
		return fmt.Errorf("git add failed: %s", addResult.Stderr)
	}

	result, err := p.Exec(ctx, "git", "commit", "-m", message)
	if err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("git commit failed: %s", result.Stderr)
	}

	return nil
}

// GitStatus returns the current git status.
func (p *Project) GitStatus(ctx context.Context) (string, error) {
	result, err := p.Exec(ctx, "git", "status", "--short")
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	return strings.TrimSpace(result.Stdout), nil
}

// GitDiff returns the diff of uncommitted changes.
func (p *Project) GitDiff(ctx context.Context) (string, error) {
	result, err := p.Exec(ctx, "git", "diff")
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return result.Stdout, nil
}
