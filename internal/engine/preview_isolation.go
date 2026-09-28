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
	previewLookPath      = exec.LookPath
	previewGOOS          = runtime.GOOS
	previewUserHome      = os.UserHomeDir
	previewGetenv        = os.Getenv
	previewBwrapSelfTest = func(bwrapPath string) error {
		cmd := exec.Command(bwrapPath, "--ro-bind", "/", "/", "--unshare-pid", "true")
		output, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}

		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = strings.TrimSpace(err.Error())
		}
		if detail == "" {
			detail = "unknown bubblewrap startup failure"
		}
		if len(detail) > 240 {
			detail = detail[:240] + "..."
		}
		return fmt.Errorf("project script preview sandbox is unavailable (%s); set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)", detail)
	}
	previewUnsafe = func() bool {
		return os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC") == "1"
	}
)

// wrapPreviewProjectCommand wraps project-defined preview scripts in an isolated
// runtime. By default this requires bubblewrap on Linux. Users can explicitly
// bypass this with MAKEWAND_UNSAFE_HOST_EXEC=1 once the one-time host execution
// acknowledgment carried by auth has been completed; the environment variable
// alone never enables host execution.
func wrapPreviewProjectCommand(projectPath, command string, args []string, auth UnsafeHostExecAuthorization) (string, []string, error) {
	unsafeRequested := previewUnsafe()
	if unsafeRequested && auth.Acknowledged {
		auth.audit(UnsafeHostExecEvent{
			Context: "preview",
			Command: command,
			Args:    append([]string(nil), args...),
			Dir:     projectPath,
		})
		return command, args, nil
	}
	if previewGOOS != "linux" {
		return "", nil, fmt.Errorf("project script preview requires sandbox isolation on %s; %s", previewGOOS, unsafeBypassHint(unsafeRequested))
	}

	bwrapPath, err := previewLookPath("bwrap")
	if err != nil {
		return "", nil, fmt.Errorf("project script preview requires bubblewrap (bwrap); install bwrap or %s", unsafeBypassHint(unsafeRequested))
	}
	if err := previewBwrapSelfTest(bwrapPath); err != nil {
		msg := strings.TrimSpace(err.Error())
		if unsafeRequested {
			msg = strings.ReplaceAll(msg, "set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)", unsafeBypassHint(true))
		}
		if !strings.Contains(msg, "MAKEWAND_UNSAFE_HOST_EXEC") {
			msg += "; " + unsafeBypassHint(unsafeRequested)
		}
		return "", nil, fmt.Errorf("%s", msg)
	}
	projectPath = filepath.Clean(projectPath)
	pathEnv := strings.TrimSpace(previewGetenv("PATH"))
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	layout := buildSandboxHomeLayout(previewHome(), projectPath, pathEnv, previewGetenv)

	wrapped := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		// Root first; fresh /proc and /dev afterwards so they overlay the
		// read-only root instead of being shadowed by it (a shadowed /dev makes
		// /dev/null read-only and breaks most tooling).
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		// tmpfs /tmp before projectPath bind so temp-dir previews stay writable and don't shadow projectPath
		"--tmpfs", "/tmp",
		// S05 defense: Mask system Unix domain sockets and host IPC runtimes
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run",
	}
	// Hide the host HOME (re-binding only toolchains, read-only) before the
	// project bind so a project under HOME stays visible and writable.
	wrapped = append(wrapped, layout.beforeWorkspace...)
	wrapped = append(wrapped, "--bind", projectPath, projectPath)
	wrapped = append(wrapped, layout.afterWorkspace...)
	if gitDir := filepath.Join(projectPath, ".git"); isRealDirOrFile(gitDir) {
		wrapped = append(wrapped, "--ro-bind", gitDir, gitDir)
	}
	wrapped = append(wrapped,
		"--chdir", projectPath,
		"--clearenv",
		"--setenv", "PATH", pathEnv,
		"--setenv", "HOME", "/tmp",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "NO_COLOR", "1",
		"--setenv", "MAKEWAND_SANDBOX", "1",
	)
	wrapped = append(wrapped, layout.setenvArgs()...)
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM"} {
		value := strings.TrimSpace(previewGetenv(key))
		if value != "" {
			wrapped = append(wrapped, "--setenv", key, value)
		}
	}
	wrapped = append(wrapped, command)
	wrapped = append(wrapped, args...)
	return bwrapPath, wrapped, nil
}

func previewHome() string {
	home, err := previewUserHome()
	if err != nil {
		return ""
	}
	return home
}

// isRealDirOrFile reports whether path is a real directory or regular file (not
// a symlink). Only such paths may be bound: bubblewrap follows symlinks.
func isRealDirOrFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
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
