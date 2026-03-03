package engine

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

var (
	previewLookPath = exec.LookPath
	previewGOOS     = runtime.GOOS
	previewUnsafe   = func() bool {
		return os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC") == "1"
	}
)

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

	wrapped := []string{
		"--die-with-parent",
		"--new-session",
		"--proc", "/proc",
		"--dev", "/dev",
		"--ro-bind", "/", "/",
		"--bind", projectPath, projectPath,
		"--chdir", projectPath,
		"--tmpfs", "/tmp",
		"--setenv", "HOME", "/tmp",
		"--setenv", "MAKEWAND_SANDBOX", "1",
		command,
	}
	wrapped = append(wrapped, args...)
	return bwrapPath, wrapped, nil
}
