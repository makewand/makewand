package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestAuditRemediation_SandboxSensitiveHomeEntries(t *testing.T) {
	required := []string{
		".ssh", ".aws", ".azure", ".gnupg", ".kube",
		".docker", ".netrc", ".npmrc", ".yarnrc", ".pypirc",
		".pgpass", ".gitconfig", ".git-credentials",
		".cargo/credentials", ".cargo/credentials.toml", ".cargo/config.toml",
		".m2/settings.xml", ".gradle/gradle.properties",
		".s3cfg", ".boto", ".oci", ".aider.conf.yml",
		".claude", ".claude-2", ".claude-3", ".claude.json",
		".codex", ".codex-2", ".codex-3",
		".gemini", ".agy", ".antigravity",
		".anthropic", ".openai", ".grok", ".aider",
		".bash_history", ".zsh_history", ".histfile", ".python_history",
		".node_repl_history", ".psql_history", ".mysql_history", ".sqlite_history",
		".lesshst", ".viminfo",
		".local/share/keyrings", ".local/share/credentials", ".local/share/makewand",
		".local/share/muse", ".local/share/fish", ".local/state",
		"server_auth.json", "state.db", "state.db-wal", "state.db-shm",
		"admin_session_secret", "users.json", "audit.jsonl", "usage.jsonl",
		".config/makewand", ".config/gcloud", ".config/gh", ".config/hub",
		".config/git/credentials", ".config/rclone", ".config/muse",
		".config/google-chrome", ".config/chromium", ".mozilla", ".pki",
	}

	for _, req := range required {
		if !slices.Contains(sandboxSensitiveHomeEntries, req) {
			t.Errorf("sandboxSensitiveHomeEntries missing required sensitive path: %s", req)
		}
	}
}

func TestAuditRemediation_WorkspaceLockPath(t *testing.T) {
	dir := t.TempDir()
	lockPath, err := workspaceLockPath(dir)
	if err != nil {
		t.Fatalf("workspaceLockPath error: %v", err)
	}

	if filepath.Base(filepath.Dir(lockPath)) != ".locks" {
		t.Errorf("lock must be in .locks directory, got: %s", lockPath)
	}

	key := dir
	hash := sha256.Sum256([]byte(key))
	expectedName := hex.EncodeToString(hash[:])[:32] + ".lock"
	if filepath.Base(lockPath) != expectedName && !strings.HasSuffix(lockPath, ".lock") {
		t.Errorf("lock name must end with .lock, got %s", filepath.Base(lockPath))
	}
}

func TestAuditRemediation_SafeGitCommand(t *testing.T) {
	ws := t.TempDir()
	fakeGitName := "git"
	if runtime.GOOS == "windows" {
		fakeGitName = "git.exe"
	}
	fakeGitPath := filepath.Join(ws, fakeGitName)
	if err := os.WriteFile(fakeGitPath, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil { //nolint:gosec // G306: fixture must be executable
		t.Fatalf("failed to create fake git: %v", err)
	}

	// 1. With PATH containing ws first
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", ws+string(filepath.ListSeparator)+origPath)

	resolved, err := resolveSafeGitBinary(ws)
	if err == nil {
		if resolved == fakeGitPath || isInsideDir(resolved, ws) {
			t.Errorf("resolveSafeGitBinary resolved to fake git inside ws: %s", resolved)
		}
	}

	// 2. With PATH containing ONLY ws
	t.Setenv("PATH", ws)
	_, errOnlyWs := resolveSafeGitBinary(ws)
	if errOnlyWs == nil {
		t.Errorf("expected error when PATH only contains fake git in ws, got nil")
	}

	// 3. Test safeGitCmd injects NoDefaultCurrentDirectoryInExePath
	t.Setenv("PATH", origPath)
	cmd, err := safeGitCmd(ws, "version")
	if err == nil {
		found := false
		for _, e := range cmd.Env {
			if strings.EqualFold(e, "NoDefaultCurrentDirectoryInExePath=1") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("safeGitCmd did not inject NoDefaultCurrentDirectoryInExePath=1: %v", cmd.Env)
		}
	}
}
