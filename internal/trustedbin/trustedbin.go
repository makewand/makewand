// Package trustedbin resolves security-critical launcher binaries (bubblewrap)
// from fixed system directories instead of $PATH.
//
// A repository-controlled directory on $PATH (an activated local virtualenv,
// node_modules/.bin, direnv) could otherwise shadow "bwrap" and run on the host
// before any confinement exists. Callers must use the returned path for both
// the availability probe and the real launch so the two cannot diverge.
package trustedbin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SystemDirs are searched in order. Only root-owned, non-group/world-writable
// files in root-owned, non-group/world-writable directories are accepted.
var SystemDirs = []string{
	"/usr/bin",
	"/bin",
	"/usr/local/bin",
	"/usr/sbin",
	"/sbin",
	"/run/current-system/sw/bin", // NixOS system profile (symlinks into /nix/store)
}

// ErrNotFound reports that no trusted copy of the binary exists.
var ErrNotFound = errors.New("trustedbin: no trusted executable found")

// OverrideEnv returns the operator override variable for name, e.g.
// MAKEWAND_BWRAP_PATH for "bwrap". The override must be an absolute path and
// may be owned by root or the current user, but never group/world writable.
func OverrideEnv(name string) string {
	return "MAKEWAND_" + strings.ToUpper(name) + "_PATH"
}

// LookPath has the exec.LookPath signature so it can replace it in injectable
// lookup variables. It never consults $PATH. The returned path is the fully
// resolved (symlink-free) file that passed validation.
func LookPath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, os.PathSeparator) {
		return "", fmt.Errorf("trustedbin: invalid binary name %q", name)
	}
	if override := strings.TrimSpace(os.Getenv(OverrideEnv(name))); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("trustedbin: %s must be an absolute path, got %q", OverrideEnv(name), override)
		}
		real, err := validate(override, true)
		if err != nil {
			return "", fmt.Errorf("trustedbin: %s rejected: %w", OverrideEnv(name), err)
		}
		return real, nil
	}
	var lastErr error
	for _, dir := range SystemDirs {
		candidate := filepath.Join(dir, name)
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		real, err := validate(candidate, false)
		if err != nil {
			lastErr = err
			continue
		}
		return real, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("%w: %s (%v)", ErrNotFound, name, lastErr)
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, name)
}

func validate(path string, allowCurrentUser bool) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real) //nolint:gosec // G703: inspecting, not opening, a candidate launcher
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", real)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", real)
	}
	if err := checkOwnership(real, info, allowCurrentUser); err != nil {
		return "", err
	}
	dir := filepath.Dir(real)
	dirInfo, err := os.Stat(dir) //nolint:gosec // G703: inspecting the launcher directory
	if err != nil {
		return "", err
	}
	if err := checkOwnership(dir, dirInfo, allowCurrentUser); err != nil {
		return "", err
	}
	return real, nil
}
