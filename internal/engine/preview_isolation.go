package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	previewLookPath = exec.LookPath
	previewGOOS     = runtime.GOOS
	previewUserHome = os.UserHomeDir
	previewGetenv   = os.Getenv
	previewUnsafe   = func() bool {
		return os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC") == "1"
	}
)

var previewSensitiveHomeEntries = []string{
	".ssh",
	".aws",
	".gnupg",
	".kube",
	".config",
	".netrc",
	".npmrc",
	".pypirc",
}

// wrapPreviewProjectCommand wraps project-defined preview scripts in an isolated
// runtime. By default this requires bubblewrap on Linux. Users can explicitly
// bypass this with MAKEWAND_UNSAFE_HOST_EXEC=1.
func wrapPreviewProjectCommand(projectPath, command string, args []string) (string, []string, error) {
	if previewUnsafe() {
		return command, args, nil
	}
	if previewGOOS != "linux" {
		return "", nil, fmt.Errorf("project script preview requires sandbox isolation on %s; set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)", previewGOOS)
	}

	bwrapPath, err := previewLookPath("bwrap")
	if err != nil {
		return "", nil, fmt.Errorf("project script preview requires bubblewrap (bwrap); install bwrap or set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)")
	}
	projectPath = filepath.Clean(projectPath)
	pathEnv := strings.TrimSpace(previewGetenv("PATH"))
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	wrapped := []string{
		"--die-with-parent",
		"--new-session",
		"--proc", "/proc",
		"--dev", "/dev",
		"--ro-bind", "/", "/",
		"--bind", projectPath, projectPath,
		"--chdir", projectPath,
		"--tmpfs", "/tmp",
		"--clearenv",
		"--setenv", "PATH", pathEnv,
		"--setenv", "HOME", "/tmp",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "NO_COLOR", "1",
		"--setenv", "MAKEWAND_SANDBOX", "1",
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM"} {
		value := strings.TrimSpace(previewGetenv(key))
		if value != "" {
			wrapped = append(wrapped, "--setenv", key, value)
		}
	}
	wrapped = append(wrapped, previewHomeMaskArgs(projectPath)...)
	wrapped = append(wrapped, command)
	wrapped = append(wrapped, args...)
	return bwrapPath, wrapped, nil
}

func previewHomeMaskArgs(projectPath string) []string {
	home, err := previewUserHome()
	if err != nil {
		return nil
	}
	home = strings.TrimSpace(home)
	if home == "" {
		return nil
	}
	home = filepath.Clean(home)

	// If the project lives outside HOME, hide the entire host home tree.
	if !pathWithin(home, projectPath) {
		return []string{"--tmpfs", home}
	}

	// If the project is under HOME, mask common sensitive subpaths but keep
	// the project path accessible.
	out := make([]string, 0, len(previewSensitiveHomeEntries)*2)
	for _, entry := range previewSensitiveHomeEntries {
		target := filepath.Join(home, entry)
		if pathWithin(target, projectPath) {
			continue
		}
		out = append(out, "--tmpfs", target)
	}
	return out
}

func pathWithin(base, target string) bool {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
