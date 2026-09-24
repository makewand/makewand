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

var safeGitFlags = []string{
	"-c", "diff.tool=",
	"-c", "core.fsmonitor=",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.attributesFile=/dev/null",
	"-c", "core.pager=cat",
	"-c", "commit.gpgsign=false",
}

const safeGitAttrSource = "--attr-source=4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// GitCommit stages all changes and creates a commit.
func (p *Project) GitCommit(ctx context.Context, message string) error {
	addArgs := append([]string{"--no-pager", safeGitAttrSource}, safeGitFlags...)
	addArgs = append(addArgs, "add", "-A")
	addResult, err := p.Exec(ctx, "git", addArgs...)
	if err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if addResult.ExitCode != 0 {
		return fmt.Errorf("git add failed: %s", addResult.Stderr)
	}

	commitArgs := append([]string{"--no-pager", safeGitAttrSource}, safeGitFlags...)
	commitArgs = append(commitArgs, "commit", "-m", message)
	result, err := p.Exec(ctx, "git", commitArgs...)
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
	statusArgs := append([]string{"--no-pager", safeGitAttrSource}, safeGitFlags...)
	statusArgs = append(statusArgs, "status", "--short")
	result, err := p.Exec(ctx, "git", statusArgs...)
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	return strings.TrimSpace(result.Stdout), nil
}

// GitDiff returns the diff of uncommitted changes.
func (p *Project) GitDiff(ctx context.Context) (string, error) {
	diffArgs := append([]string{"--no-pager", safeGitAttrSource}, safeGitFlags...)
	diffArgs = append(diffArgs, "diff", "--no-ext-diff", "--no-textconv")
	result, err := p.Exec(ctx, "git", diffArgs...)
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return result.Stdout, nil
}
