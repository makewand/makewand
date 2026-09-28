package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// shieldGitAttributes temporarily masks any info/attributes files across primary
// and linked worktrees to prevent host clean/smudge filters from executing.
func shieldGitAttributes(projectPath string) func() {
	var restorations []func()
	var gitDirs []string

	for cur := projectPath; cur != "" && cur != filepath.Dir(cur); cur = filepath.Dir(cur) {
		gitPath := filepath.Join(cur, ".git")
		fi, err := os.Stat(gitPath)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			gitDirs = append(gitDirs, gitPath)
		} else {
			content, err := os.ReadFile(gitPath)
			if err == nil {
				txt := strings.TrimSpace(string(content))
				if strings.HasPrefix(txt, "gitdir:") {
					gd := strings.TrimSpace(strings.TrimPrefix(txt, "gitdir:"))
					if !filepath.IsAbs(gd) {
						gd = filepath.Join(cur, gd)
					}
					gitDirs = append(gitDirs, gd)
					//nolint:gosec // G703: Git's linked-worktree metadata deliberately refers to the common repository outside the selected worktree; this reads only its fixed commondir file.
					cdBytes, err := os.ReadFile(filepath.Join(gd, "commondir"))
					if err == nil {
						cd := strings.TrimSpace(string(cdBytes))
						if !filepath.IsAbs(cd) {
							cd = filepath.Join(gd, cd)
						}
						gitDirs = append(gitDirs, cd)
					}
				}
			}
		}
		for _, gd := range gitDirs {
			attrPath := filepath.Join(gd, "info", "attributes")
			// Self-healing: if attrPath is absent, check for any orphan .mw_shield_* from an aborted previous run
			if _, err := os.Stat(attrPath); err != nil {
				if matches, globErr := filepath.Glob(filepath.Join(gd, "info", "attributes.mw_shield_*")); globErr == nil && len(matches) > 0 {
					_ = os.Rename(matches[0], attrPath)
					for _, leftover := range matches[1:] {
						_ = os.Remove(leftover)
					}
				}
			}
			//nolint:gosec // G703: the Git administrative directory may be outside a linked worktree; the fixed attributes path is intentionally masked and restored for safe Git execution.
			if _, err := os.Stat(attrPath); err == nil {
				shieldPath := fmt.Sprintf("%s.mw_shield_%d_%d", attrPath, os.Getpid(), len(restorations))
				if err := os.Rename(attrPath, shieldPath); err == nil {
					restorations = append(restorations, func() {
						_ = os.Rename(shieldPath, attrPath)
					})
				}
			}
		}
		break
	}

	return func() {
		for _, r := range restorations {
			r()
		}
	}
}

// GitCommit stages all changes and creates a commit.
func (p *Project) GitCommit(ctx context.Context, message string) error {
	defer shieldGitAttributes(p.Path)()

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
	defer shieldGitAttributes(p.Path)()

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
	defer shieldGitAttributes(p.Path)()

	diffArgs := append([]string{"--no-pager", safeGitAttrSource}, safeGitFlags...)
	diffArgs = append(diffArgs, "diff", "--no-ext-diff", "--no-textconv")
	result, err := p.Exec(ctx, "git", diffArgs...)
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return result.Stdout, nil
}
