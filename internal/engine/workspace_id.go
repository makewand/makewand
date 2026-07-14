package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/makewand/makewand/internal/config"
)

// StableWorkspaceID returns a reproducible workspace identifier for local or
// remote session continuity. Git-backed workspaces use origin URL + subdir when
// possible; other directories fall back to a local path fingerprint unless the
// user sets MAKEWAND_WORKSPACE_ID explicitly.
func StableWorkspaceID(projectPath string) (string, error) {
	if override := strings.TrimSpace(config.WorkspaceIDOverride()); override != "" {
		return override, nil
	}

	absPath, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}

	if id, ok := gitWorkspaceID(absPath); ok {
		return id, nil
	}
	return digestWorkspaceID("path", absPath), nil
}

func gitWorkspaceID(projectPath string) (string, bool) {
	root, err := gitOutput(projectPath, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", false
	}
	remote, err := gitOutput(projectPath, "remote", "get-url", "origin")
	if err != nil || remote == "" {
		return "", false
	}
	root = strings.TrimSpace(root)
	projectPath = strings.TrimSpace(projectPath)
	rel, relErr := filepath.Rel(root, projectPath)
	if relErr != nil {
		rel = "."
	}
	return digestWorkspaceID("git", normalizeGitRemote(remote)+"|"+filepath.ToSlash(rel)), true
}

func digestWorkspaceID(prefix, value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	short := hex.EncodeToString(sum[:8])
	return fmt.Sprintf("%s-%s", prefix, short)
}

func gitOutput(projectPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", projectPath}, args...)
	out, err := exec.Command("git", cmdArgs...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func normalizeGitRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, ".git")
	remote = strings.TrimPrefix(remote, "ssh://")
	remote = strings.TrimPrefix(remote, "https://")
	remote = strings.TrimPrefix(remote, "http://")
	if at := strings.Index(remote, "@"); at >= 0 {
		remote = remote[at+1:]
	}
	return strings.ToLower(strings.ReplaceAll(remote, ":", "/"))
}
