package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var cliBwrapLookup = exec.LookPath

var cliSensitiveHomeEntries = []string{
	".ssh",
	".aws",
	".azure",
	".gnupg",
	".kube",
	".docker",
	".netrc",
	".npmrc",
	".pypirc",
	".pgpass",
	".git-credentials",
	".vault-token",
	".password-store",
	".terraform.d",
	".bash_history",
	".zsh_history",
	".histfile",
	".python_history",
	".local/share/keyrings",
}

var cliProviderCredentials = map[string][]string{
	"claude": {".claude", ".claude.json"},
	"codex":  {".codex"},
	"gemini": {".gemini", ".agy", ".antigravity"},
	"agy":    {".gemini", ".agy", ".antigravity"},
	"grok":   {".grok"},
	"aider":  {".aider"},
}

// isCLISandboxRequired reports whether execution MUST be sandboxed because the
// context is untrusted or restricted.
func isCLISandboxRequired(ctx context.Context) bool {
	if RemoteOriginFromContext(ctx) {
		return true
	}
	if os.Getenv("MAKEWAND_RESTRICTED") == "1" || os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
		return true
	}
	if os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC") == "1" {
		return false
	}
	if task, ok := TaskFromContext(ctx); ok && task == TaskReview {
		return true
	}
	return false
}

func cliPathWithin(base, target string) bool {
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

func maskSensitivePath(args []string, target, workspace string) []string {
	info, err := os.Lstat(target)
	if err != nil {
		return args
	}
	if info.Mode()&os.ModeSymlink != 0 {
		real, statErr := filepath.EvalSymlinks(target)
		if statErr != nil {
			return args // dangling symlink: nothing readable behind it
		}
		// If real target is under a masked host root and not within workspace,
		// it is already hidden by the root's tmpfs mount, leaving target dangling inside sandbox.
		for _, m := range []string{"/mnt", "/root", "/media", "/srv"} {
			if cliPathWithin(m, real) && !cliPathWithin(workspace, real) {
				return args
			}
		}
		target = real
		info, err = os.Stat(target)
		if err != nil {
			return args
		}
	}
	if target == workspace || cliPathWithin(target, workspace) {
		return args
	}
	if info.IsDir() {
		return append(args, "--tmpfs", target)
	}
	return append(args, "--ro-bind", os.DevNull, target)
}

// rebindToolchainUnderMaskedRoot ensures that targetBin and any of its symlink hops
// or containing environment directories that reside under masked roots (like /mnt or /tmp)
// are re-bound read-only so they remain executable inside the sandbox.
func rebindToolchainUnderMaskedRoot(args []string, targetBin, workspace string) []string {
	if targetBin == "" {
		return args
	}
	targetBin = filepath.Clean(targetBin)

	paths := []string{targetBin}
	dir := filepath.Dir(targetBin)
	paths = append(paths, dir)
	if base := filepath.Base(dir); base == "bin" || base == "sbin" || base == "shims" {
		paths = append(paths, filepath.Dir(dir))
	}

	var toRebind []string
	for _, p := range paths {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			real = filepath.Clean(real)
			if real != p {
				toRebind = append(toRebind, real)
			}
		}
		toRebind = append(toRebind, p)
	}

	for _, p := range toRebind {
		if p == "" || p == "/" || p == workspace || cliPathWithin(workspace, p) {
			continue
		}
		underMasked := false
		for _, m := range []string{"/mnt", "/root", "/media", "/srv"} {
			if cliPathWithin(m, p) && p != m {
				underMasked = true
				break
			}
		}
		if underMasked {
			if _, err := os.Stat(p); err == nil {
				already := false
				for i := 0; i+1 < len(args); i++ {
					if args[i] == "--ro-bind" && args[i+1] == p {
						already = true
						break
					}
				}
				if !already {
					args = append(args, "--ro-bind", p, p)
				}
			}
		}
	}
	return args
}

// wrapCLICommandWithSandbox wraps a CLI provider command with bubblewrap (bwrap).
// It isolates the execution by mounting the workspace (read-write or read-only
// for review tasks), masking host roots (/root, /mnt, /media, /srv), and masking
// sensitive host credentials in HOME while keeping the active provider's state
// accessible. In untrusted or restricted contexts, it fails closed if bwrap is missing.
func wrapCLICommandWithSandbox(ctx context.Context, provider string, cmd *exec.Cmd) (*exec.Cmd, error) {
	if cmd == nil {
		return nil, errors.New("cannot sandbox nil command")
	}

	bwrapPath, err := cliBwrapLookup("bwrap")
	if err != nil || os.Getenv("MAKEWAND_NO_BWRAP") == "1" {
		if isCLISandboxRequired(ctx) {
			return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0,
				"bubblewrap (bwrap) required for sandboxed execution in untrusted or restricted context, but not found", err)
		}
		return cmd, nil
	}

	workspace := cmd.Dir
	if workspace == "" {
		if wd, err := os.Getwd(); err == nil {
			workspace = wd
		} else {
			workspace = "."
		}
	}
	if abs, err := filepath.Abs(workspace); err == nil {
		workspace = abs
	}
	if realWS, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = realWS
	}
	workspace = filepath.Clean(workspace)
	cmd.Dir = workspace

	isReview := false
	if task, ok := TaskFromContext(ctx); ok && task == TaskReview {
		isReview = true
	}

	bwrapArgs := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		// Network is NOT unshared: external API access is required by CLI providers.
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", "/tmp", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run",
	}

	// Mask host roots (/root, /media, /srv, /mnt)
	for _, r := range []string{"/root", "/media", "/srv"} {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() && workspace != r {
			bwrapArgs = append(bwrapArgs, "--tmpfs", r)
		}
	}
	if fi, err := os.Stat("/mnt"); err == nil && fi.IsDir() && workspace != "/mnt" {
		bwrapArgs = append(bwrapArgs, "--tmpfs", "/mnt")
	}

	// Mount workspace (after host roots /tmpfs mounts so it overlays correctly)
	if isReview {
		bwrapArgs = append(bwrapArgs, "--ro-bind", workspace, workspace)
	} else {
		bwrapArgs = append(bwrapArgs, "--bind", workspace, workspace)
	}

	// Re-bind target binary and any symlink targets (essential if binary or its environment is under /tmp or /mnt)
	targetBin := cmd.Path
	if len(cmd.Args) > 0 && cmd.Args[0] != "" {
		targetBin = cmd.Args[0]
	}
	if !filepath.IsAbs(targetBin) {
		if resolved, err := exec.LookPath(targetBin); err == nil {
			targetBin = resolved
		}
	}
	bwrapArgs = rebindToolchainUnderMaskedRoot(bwrapArgs, targetBin, workspace)

	// Mask sensitive credentials in HOME while keeping the active provider's state
	home := os.Getenv("HOME")
	if home != "" {
		home = filepath.Clean(home)
		p := strings.ToLower(provider)
		p = strings.TrimSuffix(p, "-cli")

		allowedCreds := cliProviderCredentials[p]
		isAllowed := func(entry string) bool {
			for _, allowed := range allowedCreds {
				if entry == allowed || strings.HasPrefix(entry, allowed+"/") {
					return true
				}
			}
			return false
		}

		var toMask []string
		toMask = append(toMask, cliSensitiveHomeEntries...)
		for prov, creds := range cliProviderCredentials {
			if prov == p {
				continue
			}
			for _, c := range creds {
				if !isAllowed(c) {
					toMask = append(toMask, c)
				}
			}
		}

		for _, entry := range toMask {
			target := filepath.Join(home, filepath.FromSlash(entry))
			if target == workspace || cliPathWithin(target, workspace) {
				continue
			}
			bwrapArgs = maskSensitivePath(bwrapArgs, target, workspace)
		}
	}

	bwrapArgs = append(bwrapArgs, "--chdir", workspace)
	bwrapArgs = append(bwrapArgs, "--")

	if len(cmd.Args) > 0 {
		binArg := cmd.Args[0]
		if !filepath.IsAbs(binArg) {
			if targetBin != "" && filepath.IsAbs(targetBin) {
				binArg = targetBin
			} else if cmd.Path != "" && filepath.IsAbs(cmd.Path) {
				binArg = cmd.Path
			}
		}
		bwrapArgs = append(bwrapArgs, binArg)
		bwrapArgs = append(bwrapArgs, cmd.Args[1:]...)
	} else if cmd.Path != "" {
		bwrapArgs = append(bwrapArgs, cmd.Path)
	}

	wrapped := exec.CommandContext(ctx, bwrapPath, bwrapArgs...)
	wrapped.Dir = cmd.Dir
	wrapped.Env = cmd.Env
	wrapped.Stdin = cmd.Stdin
	wrapped.Stdout = cmd.Stdout
	wrapped.Stderr = cmd.Stderr
	wrapped.ExtraFiles = cmd.ExtraFiles

	return wrapped, nil
}
