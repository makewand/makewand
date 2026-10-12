package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
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

	defaultGitignore := `node_modules/
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
	gitignorePath := filepath.Join(p.Path, ".gitignore")
	existing, err := os.ReadFile(gitignorePath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := p.WriteFile(".gitignore", defaultGitignore); err != nil {
				return fmt.Errorf("write .gitignore: %w", err)
			}
			return nil
		}
		return fmt.Errorf("read existing .gitignore: %w", err)
	}

	existingStr := string(existing)
	lines := strings.Split(existingStr, "\n")
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		seen[strings.TrimSpace(trimmed)] = true
	}

	var missing []string
	for _, entry := range strings.Split(strings.TrimSpace(defaultGitignore), "\n") {
		entry = strings.TrimSpace(strings.TrimRight(entry, "\r"))
		if entry != "" && !seen[entry] {
			missing = append(missing, entry)
		}
	}

	if len(missing) > 0 {
		merged := existingStr
		if len(merged) > 0 && !strings.HasSuffix(merged, "\n") {
			merged += "\n"
		}
		merged += strings.Join(missing, "\n") + "\n"
		if err := p.WriteFile(".gitignore", merged); err != nil {
			return fmt.Errorf("update .gitignore: %w", err)
		}
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
// It acquires an attributes.lock file lock to prevent multi-session concurrency race conditions.
func shieldGitAttributes(projectPath string) (func(), error) {
	var restorations []func()
	var rawGitDirs []string

	cleanup := func() {
		for i := len(restorations) - 1; i >= 0; i-- {
			restorations[i]()
		}
	}

	for cur := projectPath; cur != "" && cur != filepath.Dir(cur); cur = filepath.Dir(cur) {
		gitPath := filepath.Join(cur, ".git")
		fi, err := os.Stat(gitPath)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			rawGitDirs = append(rawGitDirs, gitPath)
		} else {
			content, err := os.ReadFile(gitPath)
			if err == nil {
				txt := strings.TrimSpace(string(content))
				if strings.HasPrefix(txt, "gitdir:") {
					gd := strings.TrimSpace(strings.TrimPrefix(txt, "gitdir:"))
					if !filepath.IsAbs(gd) {
						gd = filepath.Join(cur, gd)
					}
					rawGitDirs = append(rawGitDirs, gd)
					//nolint:gosec // G703: Git's linked-worktree metadata deliberately refers to the common repository outside the selected worktree; this reads only its fixed commondir file.
					cdBytes, err := os.ReadFile(filepath.Join(gd, "commondir"))
					if err == nil {
						cd := strings.TrimSpace(string(cdBytes))
						if !filepath.IsAbs(cd) {
							cd = filepath.Join(gd, cd)
						}
						rawGitDirs = append(rawGitDirs, cd)
					}
				}
			}
		}
		break
	}

	// Deduplicate and canonicalize git directories to eliminate deadlocks
	seen := make(map[string]bool)
	var gitDirs []string
	for _, dir := range rawGitDirs {
		clean := filepath.Clean(dir)
		if realPath, err := filepath.EvalSymlinks(clean); err == nil {
			clean = realPath
		}
		if !seen[clean] {
			seen[clean] = true
			gitDirs = append(gitDirs, clean)
		}
	}
	sort.Strings(gitDirs)

	for _, gd := range gitDirs {
		infoDir := filepath.Join(gd, "info")
		attrPath := filepath.Join(infoDir, "attributes")
		lockPath := filepath.Join(infoDir, "attributes.mw_gate")

		needsShield := false
		//nolint:gosec // G703: checking attributes file in git administrative directory.
		if _, err := os.Stat(attrPath); err == nil {
			needsShield = true
		} else if _, err := os.Stat(lockPath); err == nil {
			needsShield = true
		} else {
			if m1, _ := filepath.Glob(filepath.Join(infoDir, "attributes.mw_shield_*")); len(m1) > 0 {
				needsShield = true
			} else if m2, _ := filepath.Glob(filepath.Join(infoDir, "attributes.makewand_shield_*")); len(m2) > 0 {
				needsShield = true
			}
		}
		if !needsShield {
			continue
		}

		//nolint:gosec // G703: ensure git administrative info directory exists for attributes shielding.
		if err := os.MkdirAll(infoDir, 0o700); err != nil {
			cleanup()
			return nil, fmt.Errorf("create git info dir: %w", err)
		}

		//nolint:gosec // G304: lockPath is strictly inside the git administrative info directory.
		lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("open attributes lock: %w", err)
		}

		deadline := time.Now().Add(60 * time.Second)
		locked := false
		for {
			ok, lockErr := tryApplyLock(lockFile)
			if lockErr != nil {
				_ = lockFile.Close()
				cleanup()
				return nil, fmt.Errorf("lock attributes file: %w", lockErr)
			}
			if ok {
				locked = true
				break
			}
			if time.Now().After(deadline) {
				_ = lockFile.Close()
				cleanup()
				return nil, fmt.Errorf("timed out acquiring attributes lock: %s", lockPath)
			}
			time.Sleep(10 * time.Millisecond)
		}

		if !locked {
			_ = lockFile.Close()
			cleanup()
			return nil, fmt.Errorf("failed to acquire attributes lock: %s", lockPath)
		}

		// Under lock: check for any orphan .mw_shield_* or .makewand_shield_* from an aborted previous run
		var orphans []string
		if matches, globErr := filepath.Glob(filepath.Join(infoDir, "attributes.mw_shield_*")); globErr == nil && len(matches) > 0 {
			orphans = append(orphans, matches...)
		}
		if matches, globErr := filepath.Glob(filepath.Join(infoDir, "attributes.makewand_shield_*")); globErr == nil && len(matches) > 0 {
			orphans = append(orphans, matches...)
		}
		if len(orphans) > 0 {
			sort.Strings(orphans)
			//nolint:gosec // G703: self-healing check for attributes file in git administrative directory.
			if _, err := os.Stat(attrPath); err != nil {
				//nolint:gosec // G703: restoring orphaned attributes shield file.
				_ = os.Rename(orphans[0], attrPath)
				for _, leftover := range orphans[1:] {
					//nolint:gosec // G703: cleaning up extra orphaned shield files.
					_ = os.Remove(leftover)
				}
			} else {
				// attrPath already exists; stale orphans should be pruned
				for _, leftover := range orphans {
					//nolint:gosec // G703: cleaning up stale orphaned shield files.
					_ = os.Remove(leftover)
				}
			}
		}

		// Under lock: check if attributes exists and needs shielding
		//nolint:gosec // G703: checking attributes file in git administrative directory under lock.
		if _, err := os.Stat(attrPath); err == nil {
			shieldPath := fmt.Sprintf("%s.mw_shield_%d_%d", attrPath, os.Getpid(), len(restorations))
			if err := os.Rename(attrPath, shieldPath); err != nil {
				_ = unlockApply(lockFile)
				_ = lockFile.Close()
				cleanup()
				return nil, fmt.Errorf("rename attributes file: %w", err)
			}
			// When cleaning up in reverse order:
			// 1. Rename shieldPath back to attrPath.
			// 2. Release attributes.lock and close lockFile.
			restorations = append(restorations, func() {
				_ = unlockApply(lockFile)
				_ = lockFile.Close()
			})
			restorations = append(restorations, func() {
				//nolint:gosec // G703: restoring shielded attributes file.
				_ = os.Rename(shieldPath, attrPath)
			})
		} else {
			// No attributes file to shield; release lock immediately so operations on repositories
			// without attributes do not block each other during git execution.
			_ = unlockApply(lockFile)
			_ = lockFile.Close()
		}
	}

	return cleanup, nil
}

// GitCommit stages all changes and creates a commit.
func (p *Project) GitCommit(ctx context.Context, message string) error {
	cleanup, err := shieldGitAttributes(p.Path)
	if err != nil {
		return fmt.Errorf("shield git attributes: %w", err)
	}
	defer cleanup()

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
	cleanup, err := shieldGitAttributes(p.Path)
	if err != nil {
		return "", fmt.Errorf("shield git attributes: %w", err)
	}
	defer cleanup()

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
	cleanup, err := shieldGitAttributes(p.Path)
	if err != nil {
		return "", fmt.Errorf("shield git attributes: %w", err)
	}
	defer cleanup()

	diffArgs := append([]string{"--no-pager", safeGitAttrSource}, safeGitFlags...)
	diffArgs = append(diffArgs, "diff", "--no-ext-diff", "--no-textconv")
	result, err := p.Exec(ctx, "git", diffArgs...)
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return result.Stdout, nil
}

func isInsideDir(path, parent string) bool {
	if parent == "" {
		return false
	}
	p := filepath.Clean(path)
	par := filepath.Clean(parent)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
		par = strings.ToLower(par)
	}
	if p == par {
		return true
	}
	rel, err := filepath.Rel(par, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isExecutable(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // G703: stat of a fixed PATH candidate, never opened
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

// resolveSafeGitBinary resolves the git executable safely, ensuring that it is
// not located inside the specified workspace directory (to prevent cwd binary hijacking).
func resolveSafeGitBinary(workspace string) (string, error) {
	cleanWS := filepath.Clean(workspace)
	if realWS, err := filepath.EvalSymlinks(cleanWS); err == nil {
		cleanWS = realWS
	}

	pathEnv := os.Getenv("PATH")
	var cleanEntries []string
	for _, entry := range filepath.SplitList(pathEnv) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		entryClean := filepath.Clean(entry)
		if realEntry, err := filepath.EvalSymlinks(entryClean); err == nil {
			entryClean = realEntry
		}
		if workspace != "" && isInsideDir(entryClean, cleanWS) {
			continue
		}
		cleanEntries = append(cleanEntries, entry)
	}

	var found string
	for _, dir := range cleanEntries {
		candidate := filepath.Join(dir, "git")
		if runtime.GOOS == "windows" {
			for _, ext := range []string{".exe", ".cmd", ".bat"} {
				target := candidate + ext
				if isExecutable(target) {
					found = target
					break
				}
			}
			if found == "" && isExecutable(candidate) {
				found = candidate
			}
		} else if isExecutable(candidate) {
			found = candidate
			break
		}
		if found != "" {
			break
		}
	}

	if found == "" {
		if rawFound, err := exec.LookPath("git"); err == nil {
			rawClean := filepath.Clean(rawFound)
			if realRaw, err := filepath.EvalSymlinks(rawClean); err == nil {
				rawClean = realRaw
			}
			if workspace != "" && isInsideDir(rawClean, cleanWS) {
				return "", fmt.Errorf("refusing to execute git binary found inside workspace cwd: %s", rawFound)
			}
		}
		return "", fmt.Errorf("safe git binary not found in PATH")
	}

	foundClean := filepath.Clean(found)
	if realFound, err := filepath.EvalSymlinks(foundClean); err == nil {
		foundClean = realFound
	}
	if workspace != "" && isInsideDir(foundClean, cleanWS) {
		return "", fmt.Errorf("refusing to execute git binary found inside workspace cwd: %s", found)
	}

	return foundClean, nil
}

func safeGitCmd(workspace string, args ...string) (*exec.Cmd, error) {
	gitBin, err := resolveSafeGitBinary(workspace)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = workspace
	env := os.Environ()
	hasNoDefault := false
	for _, e := range env {
		if strings.HasPrefix(strings.ToLower(e), "nodefaultcurrentdirectoryinexepath=") {
			hasNoDefault = true
			break
		}
	}
	if !hasNoDefault {
		env = append(env, "NoDefaultCurrentDirectoryInExePath=1")
	}
	cmd.Env = env
	return cmd, nil
}
