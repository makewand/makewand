package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Candidate verification executes AI-generated code (tests, builds, dependency
// installs). By default those commands must run inside a bubblewrap sandbox:
// read-only root, writable workspace bind, cleared environment, tmpfs /tmp,
// masked sensitive HOME entries, and - unlike the preview sandbox - no network
// for test/build/compile steps. Dependency installs keep network access (they
// have to reach package registries) but run with the same filesystem isolation.
// Without working isolation, verification fails closed: no command executes
// unless the user explicitly opts into host execution with
// MAKEWAND_UNSAFE_HOST_EXEC=1 (the same escape hatch the preview path uses).

var (
	verifyLookPath      = exec.LookPath
	verifyGOOS          = runtime.GOOS
	verifyUserHome      = os.UserHomeDir
	verifyGetenv        = os.Getenv
	verifyBwrapSelfTest = func(bwrapPath string) error {
		cmd := exec.Command(bwrapPath, "--ro-bind", "/", "/", "--unshare-net", "true")
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
		return fmt.Errorf("candidate verification sandbox is unavailable (%s); set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)", detail)
	}
	verifyUnsafe = func() bool {
		return os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC") == "1"
	}
)

// verifyExecMode describes how verification commands may execute on this host.
type verifyExecMode int

const (
	verifyExecIsolated verifyExecMode = iota
	verifyExecUnsafeHost
)

type verifyExecEnvironment struct {
	mode      verifyExecMode
	bwrapPath string
}

// resolveVerifyExecEnvironment decides how verification commands can run.
// It fails closed: without working bubblewrap isolation, and without the
// explicit MAKEWAND_UNSAFE_HOST_EXEC=1 opt-in, no command may execute.
func resolveVerifyExecEnvironment() (verifyExecEnvironment, error) {
	if verifyUnsafe() {
		return verifyExecEnvironment{mode: verifyExecUnsafeHost}, nil
	}
	if verifyGOOS != "linux" {
		return verifyExecEnvironment{}, fmt.Errorf("candidate verification requires sandbox isolation on %s; set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)", verifyGOOS)
	}

	bwrapPath, err := verifyLookPath("bwrap")
	if err != nil {
		return verifyExecEnvironment{}, fmt.Errorf("candidate verification requires bubblewrap (bwrap); install bwrap or set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)")
	}
	if err := verifyBwrapSelfTest(bwrapPath); err != nil {
		msg := strings.TrimSpace(err.Error())
		if !strings.Contains(msg, "MAKEWAND_UNSAFE_HOST_EXEC=1") {
			msg += "; set MAKEWAND_UNSAFE_HOST_EXEC=1 to bypass (unsafe)"
		}
		return verifyExecEnvironment{}, fmt.Errorf("%s", msg)
	}
	return verifyExecEnvironment{mode: verifyExecIsolated, bwrapPath: bwrapPath}, nil
}

// VerificationIsolationActive reports whether restricted verification commands
// will run inside the bubblewrap sandbox on this host.
func VerificationIsolationActive() bool {
	env, err := resolveVerifyExecEnvironment()
	return err == nil && env.mode == verifyExecIsolated
}

// RestrictedExecAutoApprovable reports whether safe/autopilot approval modes may
// run restricted plans without asking the user: either bubblewrap isolation is
// active, or the user explicitly opted into host execution via
// MAKEWAND_UNSAFE_HOST_EXEC=1.
func RestrictedExecAutoApprovable() bool {
	_, err := resolveVerifyExecEnvironment()
	return err == nil
}

// RestrictedExecIsolationError returns why restricted commands cannot execute
// under isolation, or nil when isolation is active or the user opted into host
// execution.
func RestrictedExecIsolationError() error {
	_, err := resolveVerifyExecEnvironment()
	return err
}

// sandboxHomeRelPath is the workspace-relative HOME used inside the
// verification sandbox. Keeping HOME inside the (writable, bind-mounted)
// workspace lets dependency installs populate tool caches (go module cache,
// ~/.npm, pip --user site, ~/.cargo) that the network-isolated test step can
// then reuse. The path lives under .makewand, which project scans and clones
// already ignore.
const sandboxHomeRelPath = ".makewand/sandbox-home"

// wrapVerificationCommand wraps a verification command in bubblewrap. The
// workspace is the only writable host path; everything else is read-only with
// tmpfs /tmp, a cleared environment, and sensitive HOME entries masked. When
// allowNetwork is false the sandbox additionally unshares the network
// namespace, which is the default for test/build/compile steps.
func wrapVerificationCommand(bwrapPath, workspacePath, command string, args []string, allowNetwork bool) (string, []string) {
	workspacePath = filepath.Clean(workspacePath)
	pathEnv := strings.TrimSpace(verifyGetenv("PATH"))
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	sandboxHome := filepath.Join(workspacePath, filepath.FromSlash(sandboxHomeRelPath))

	wrapped := []string{
		"--die-with-parent",
		"--new-session",
		// Root first; fresh /proc and /dev afterwards so they overlay the
		// read-only root instead of being shadowed by it.
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		// tmpfs /tmp before the workspace bind so temp-dir verification clones
		// (which usually live under /tmp) stay writable inside the sandbox.
		"--tmpfs", "/tmp",
		"--bind", workspacePath, workspacePath,
		"--chdir", workspacePath,
		"--clearenv",
		"--setenv", "PATH", pathEnv,
		"--setenv", "HOME", sandboxHome,
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "NO_COLOR", "1",
		"--setenv", "MAKEWAND_SANDBOX", "1",
	}
	if !allowNetwork {
		wrapped = append(wrapped, "--unshare-net")
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM"} {
		value := strings.TrimSpace(verifyGetenv(key))
		if value != "" {
			wrapped = append(wrapped, "--setenv", key, value)
		}
	}
	wrapped = append(wrapped, verifyHomeMaskArgs(workspacePath)...)
	wrapped = append(wrapped, command)
	wrapped = append(wrapped, args...)
	return bwrapPath, wrapped
}

func verifyHomeMaskArgs(workspacePath string) []string {
	home, err := verifyUserHome()
	if err != nil {
		return nil
	}
	home = strings.TrimSpace(home)
	if home == "" {
		return nil
	}
	home = filepath.Clean(home)

	// If the workspace lives outside HOME, hide the entire host home tree.
	if !pathWithin(home, workspacePath) {
		return []string{"--tmpfs", home}
	}

	// If the workspace is under HOME, mask common sensitive subpaths but keep
	// the workspace path accessible.
	out := make([]string, 0, len(previewSensitiveHomeEntries)*2)
	for _, entry := range previewSensitiveHomeEntries {
		target := filepath.Join(home, entry)
		if pathWithin(target, workspacePath) {
			continue
		}
		out = append(out, "--tmpfs", target)
	}
	return out
}
