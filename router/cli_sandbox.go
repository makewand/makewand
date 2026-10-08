package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	cliBwrapLookup = exec.LookPath
	cliBwrapProbe  = defaultCLIBwrapProbe
)

var (
	cliBwrapProbeOnce sync.Once
	cliBwrapProbeErr  error
)

func defaultCLIBwrapProbe(bwrapPath string) error {
	if fi, err := os.Stat(bwrapPath); err != nil || fi.IsDir() {
		// Mocked or non-existent binary path in unit tests bypasses execution probe
		return nil
	}
	cliBwrapProbeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		probe := exec.CommandContext(ctx, bwrapPath, "--ro-bind", "/", "/", "true")
		if err := probe.Run(); err != nil {
			cliBwrapProbeErr = fmt.Errorf("bubblewrap probe failed (unprivileged user namespaces may be restricted or disabled): %w", err)
		}
	})
	return cliBwrapProbeErr
}

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
	".local/share/muse",
	".local/share/credentials",
	"server_auth.json",
	"state.db",
	"state.db-wal",
	"state.db-shm",
	"admin_session_secret",
	"users.json",
	"audit.jsonl",
	"usage.jsonl",
	".config/makewand",
}

var cliProviderCredentials = map[string][]string{
	"claude":      {".claude", ".claude.json"},
	"codex":       {".codex"},
	"gemini":      {".gemini", ".agy", ".antigravity"},
	"agy":         {".gemini", ".agy", ".antigravity"},
	"antigravity": {".gemini", ".agy", ".antigravity"},
	"grok":        {".grok"},
	"aider":       {".aider"},
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

// cliEnvValue uses the child environment, including an explicit empty
// environment, so sandbox mounts and the provider select the same account.
func cliEnvValue(env []string, key string) string {
	var value string
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			value = strings.TrimPrefix(entry, key+"=")
		}
	}
	return value
}

func cliConfiguredCodexHome(env []string, workspace, home string) (string, string, error) {
	selected := cliEnvValue(env, "CODEX_HOME")
	if selected == "" {
		return "", "", nil
	}
	if !cliUnambiguousAccountPath(selected) {
		return "", "", errors.New("configured CODEX_HOME has ambiguous path traversal; refusing account fallback")
	}
	if !filepath.IsAbs(selected) {
		selected = filepath.Join(workspace, selected)
	}
	selected = filepath.Clean(selected)
	// Reject forbidden lexical paths before inspecting any selected account path.
	// In particular, a workspace symlink cannot select an outside account.
	homes := []string{home}
	workspaces := []string{workspace}
	protected := cliCodexProtectedPaths(homes)
	if err := cliValidateCodexHome(selected, workspaces, homes, protected); err != nil {
		return "", "", err
	}
	for _, base := range []string{home, workspace} {
		if base == "" {
			continue
		}
		if realBase, err := filepath.EvalSymlinks(base); err == nil {
			if base == home {
				homes = append(homes, realBase)
			} else {
				workspaces = append(workspaces, realBase)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("configured CODEX_HOME boundary is unavailable; refusing account fallback")
		}
	}
	protected = cliCodexProtectedPaths(homes)
	// A foreign credential directory can itself be a symlink to an outside path.
	// Its canonical target must remain hidden when the selected account is bound.
	for _, path := range append([]string(nil), protected...) {
		if realPath, err := filepath.EvalSymlinks(path); err == nil {
			protected = append(protected, realPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("configured CODEX_HOME boundary is unavailable; refusing account fallback")
		}
	}
	real, err := cliResolveCodexDirectory(selected, func(path string) error {
		return cliValidateCodexHome(path, workspaces, homes, protected)
	})
	if err != nil {
		return "", "", err
	}
	return selected, real, nil
}

func cliCodexProtectedPaths(homes []string) []string {
	entries := append([]string(nil), cliSensitiveHomeEntries...)
	for provider, credentials := range cliProviderCredentials {
		if provider != "codex" {
			entries = append(entries, credentials...)
		}
	}
	var protected []string
	for _, home := range homes {
		if home != "" {
			for _, entry := range entries {
				protected = append(protected, filepath.Join(home, filepath.FromSlash(entry)))
			}
		}
	}
	return protected
}

func cliValidateCodexHome(path string, workspaces, homes, protected []string) error {
	if !filepath.IsAbs(path) || filepath.VolumeName(path) != "" && strings.HasPrefix(filepath.VolumeName(path), `\\`) {
		return errors.New("CODEX_HOME must be a dedicated local provider state directory")
	}
	for _, broad := range append([]string{"/", "/home", "/root", "/mnt", "/media", "/srv", "/tmp", "/var"}, homes...) {
		if broad != "" && cliPathWithin(path, broad) {
			return errors.New("CODEX_HOME must be a dedicated provider state directory")
		}
	}
	for _, workspace := range workspaces {
		if cliPathWithin(path, workspace) || cliPathWithin(workspace, path) {
			return errors.New("CODEX_HOME must be separate from the workspace")
		}
	}
	for _, sensitive := range protected {
		if cliPathWithin(sensitive, path) || cliPathWithin(path, sensitive) {
			return errors.New("CODEX_HOME must be a dedicated provider state directory")
		}
	}
	return nil
}

func cliUnambiguousAccountPath(path string) bool {
	seenDirectory := false
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." && seenDirectory {
			return false
		}
		if component != "" && component != "." && component != ".." {
			seenDirectory = true
		}
	}
	return true
}

// cliResolveCodexDirectory checks each complete path before looking it up. The
// filesystem root is only a namespace anchor: validate supplies the account
// authorization boundary. Each lookup is a local component in a pinned parent,
// and a link target is authorized before following it. The final directory is
// verified through its handle rather than a second unrestricted pathname Stat.
func cliResolveCodexDirectory(path string, validate func(string) error) (string, error) {
	for links := 0; links <= 40; links++ {
		if err := validate(path); err != nil {
			return "", err
		}
		anchor := filepath.VolumeName(path) + string(os.PathSeparator)
		rel, err := filepath.Rel(anchor, path)
		if err != nil || !filepath.IsLocal(rel) {
			return "", errors.New("CODEX_HOME must be a dedicated local provider state directory")
		}
		parent, err := os.OpenRoot(anchor)
		if err != nil {
			return "", errors.New("configured CODEX_HOME is unavailable; refusing account fallback")
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		resolved := anchor
		var nextPath string
		for i, component := range parts {
			if !filepath.IsLocal(component) || component == "." || strings.ContainsAny(component, `/\`) {
				_ = parent.Close()
				return "", errors.New("CODEX_HOME contains an invalid directory component")
			}
			info, err := parent.Lstat(component)
			if err != nil {
				_ = parent.Close()
				return "", errors.New("configured CODEX_HOME is unavailable; refusing account fallback")
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := parent.Readlink(component)
				_ = parent.Close()
				if err != nil {
					return "", errors.New("configured CODEX_HOME is unavailable; refusing account fallback")
				}
				// Cleaning "dir/link/../account" would skip the physical link
				// traversal and can select a different account. Only leading
				// parent navigation is safe to resolve against our pinned parent.
				if !cliUnambiguousAccountPath(target) {
					return "", errors.New("configured CODEX_HOME has ambiguous symbolic link traversal; refusing account fallback")
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(resolved, target)
				}
				nextPath = filepath.Join(append([]string{target}, parts[i+1:]...)...)
				break
			}
			if !info.IsDir() {
				_ = parent.Close()
				return "", errors.New("configured CODEX_HOME is not a provider state directory")
			}
			next, err := parent.OpenRoot(component)
			_ = parent.Close()
			if err != nil {
				return "", errors.New("configured CODEX_HOME is unavailable; refusing account fallback")
			}
			opened, err := next.Stat(".")
			if err != nil || !os.SameFile(info, opened) {
				_ = next.Close()
				return "", errors.New("configured CODEX_HOME changed during validation; refusing account fallback")
			}
			parent = next
			resolved = filepath.Join(resolved, component)
		}
		if nextPath != "" {
			path = filepath.Clean(nextPath)
			continue
		}
		_ = parent.Close()
		return resolved, nil
	}
	return "", errors.New("configured CODEX_HOME has too many symbolic links; refusing account fallback")
}

// Construct mask targets only from a fixed credential entry in HOME or from
// cliConfiguredCodexHome's dedicated-account authorization, never directly from
// a request pathname.
type cliCredentialMaskTarget struct{ path string }

func cliHomeMaskTarget(home, entry string) (cliCredentialMaskTarget, error) {
	local := filepath.FromSlash(entry)
	if !filepath.IsLocal(local) {
		return cliCredentialMaskTarget{}, errors.New("invalid credential mask entry")
	}
	allowed := false
	for _, candidate := range cliSensitiveHomeEntries {
		allowed = allowed || entry == candidate
	}
	for _, entries := range cliProviderCredentials {
		for _, candidate := range entries {
			allowed = allowed || entry == candidate
		}
	}
	if !allowed {
		return cliCredentialMaskTarget{}, errors.New("invalid credential mask entry")
	}
	return cliCredentialMaskTarget{filepath.Join(home, local)}, nil
}

func cliMaskLinkIsUnambiguous(link string) bool {
	if os.PathSeparator != '\\' || filepath.IsAbs(link) {
		return true
	}
	// A Windows drive-relative or rooted-but-not-absolute target cannot be
	// interpreted relative to the pinned parent without selecting another path.
	return filepath.VolumeName(link) == "" && (len(link) == 0 || !os.IsPathSeparator(link[0]))
}

// cliInspectMaskTarget follows authorized credential aliases through pinned
// parents. The volume root is a namespace anchor, not an account allowlist.
// Every filesystem operation receives a single local component; symlink target
// navigation is handled physically rather than cleaned across another symlink.
func cliInspectMaskTarget(target cliCredentialMaskTarget) (string, os.FileInfo, error) {
	path, err := filepath.Abs(target.path)
	if err != nil {
		return "", nil, err
	}
	var roots []*os.Root
	var paths []string
	closeRoots := func() {
		for _, root := range roots {
			_ = root.Close()
		}
		roots, paths = nil, nil
	}
	defer closeRoots()
	openAnchor := func(path string) ([]string, error) {
		closeRoots()
		anchor := filepath.VolumeName(path) + string(os.PathSeparator)
		root, err := os.OpenRoot(anchor)
		if err != nil {
			return nil, err
		}
		roots, paths = []*os.Root{root}, []string{anchor}
		return strings.Split(strings.TrimPrefix(path, anchor), string(os.PathSeparator)), nil
	}
	parts, err := openAnchor(path)
	if err != nil {
		return "", nil, err
	}
	links := 0
	for len(parts) != 0 {
		component := parts[0]
		parts = parts[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(roots) > 1 {
				_ = roots[len(roots)-1].Close()
				roots, paths = roots[:len(roots)-1], paths[:len(paths)-1]
			}
			continue
		}
		if !filepath.IsLocal(component) || filepath.Base(component) != component {
			return "", nil, errors.New("invalid credential path component")
		}
		parent := roots[len(roots)-1]
		info, err := parent.Lstat(component)
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", nil, errors.New("too many credential symbolic links")
			}
			link, err := parent.Readlink(component)
			if err != nil {
				return "", nil, err
			}
			if !cliMaskLinkIsUnambiguous(link) {
				return "", nil, errors.New("ambiguous credential symbolic link target")
			}
			var linkParts []string
			if filepath.IsAbs(link) {
				linkParts, err = openAnchor(link)
				if err != nil {
					return "", nil, err
				}
			} else {
				linkParts = strings.Split(link, string(os.PathSeparator))
			}
			parts = append(linkParts, parts...)
			continue
		}
		resolved := filepath.Join(paths[len(paths)-1], component)
		if len(parts) == 0 {
			return resolved, info, nil
		}
		if !info.IsDir() {
			return "", nil, errors.New("credential path parent is not a directory")
		}
		next, err := parent.OpenRoot(component)
		if err != nil {
			return "", nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = next.Close()
			return "", nil, errors.New("credential path changed during inspection")
		}
		roots, paths = append(roots, next), append(paths, resolved)
	}
	info, err := roots[len(roots)-1].Stat(".")
	return paths[len(paths)-1], info, err
}

func maskSensitivePath(args []string, authorized cliCredentialMaskTarget, workspace string) ([]string, error) {
	target, info, err := cliInspectMaskTarget(authorized)
	if errors.Is(err, os.ErrNotExist) {
		return args, nil // A missing file or dangling alias exposes no credential.
	}
	if err != nil {
		return nil, errors.New("credential mask target cannot be safely inspected")
	}
	for _, masked := range []string{"/home", "/mnt", "/root", "/media", "/srv"} {
		if cliPathWithin(masked, target) && !cliPathWithin(workspace, target) {
			return args, nil
		}
	}
	if target == workspace || cliPathWithin(target, workspace) {
		return args, nil
	}
	if info.IsDir() {
		return append(args, "--tmpfs", target), nil
	}
	return append(args, "--ro-bind", os.DevNull, target), nil
}

func cliMaskServerPaths(args []string, workspace string, env []string) []string {
	candidateDirs := []string{
		"/var/lib/makewand",
		"/etc/makewand",
	}
	for _, key := range []string{"MAKEWAND_DATA_DIR", "MAKEWAND_CONFIG_DIR"} {
		if val := cliEnvValue(env, key); val != "" {
			candidateDirs = append(candidateDirs, val)
		} else if val := os.Getenv(key); val != "" {
			candidateDirs = append(candidateDirs, val)
		}
	}

	for _, d := range candidateDirs {
		d = filepath.Clean(d)
		if d == "" || d == "/" {
			continue
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			if workspace != d && !cliPathWithin(workspace, d) {
				already := false
				for i := 0; i+1 < len(args); i++ {
					if args[i] == "--tmpfs" && args[i+1] == d {
						already = true
						break
					}
				}
				if !already {
					args = append(args, "--tmpfs", d)
				}
			}
		}
	}

	candidateFiles := []string{}
	for _, key := range []string{
		"MAKEWAND_SERVER_AUTH_CONFIG",
		"MAKEWAND_AUTH_CONFIG",
		"MAKEWAND_STATE_DB",
		"MAKEWAND_SERVER_STATE_DB",
		"MAKEWAND_SERVER_AUDIT_LOG",
		"MAKEWAND_SERVER_USAGE_LOG",
	} {
		if val := cliEnvValue(env, key); val != "" {
			candidateFiles = append(candidateFiles, val)
		} else if val := os.Getenv(key); val != "" {
			candidateFiles = append(candidateFiles, val)
		}
	}

	for _, f := range candidateFiles {
		f = filepath.Clean(f)
		if f == "" || f == "/" {
			continue
		}
		if fi, err := os.Stat(f); err == nil && !fi.IsDir() {
			if workspace != f && !cliPathWithin(workspace, f) {
				already := false
				for i := 0; i+2 < len(args); i++ {
					if args[i] == "--ro-bind" && args[i+1] == os.DevNull && filepath.Clean(args[i+2]) == f {
						already = true
						break
					}
				}
				if !already {
					args = append(args, "--ro-bind", os.DevNull, f)
				}
			}
		}
	}

	return args
}

func cliIsMaskedHostRoot(p string) bool {
	p = filepath.Clean(p)
	for _, m := range []string{"/", "/home", "/root", "/mnt", "/media", "/srv", "/tmp", "/var", "/etc", "/var/lib/makewand", "/etc/makewand"} {
		if p == m {
			return true
		}
	}
	return false
}

func cliPathTouchesSensitive(home, p string) bool {
	p = filepath.Clean(p)
	allSensitive := append([]string(nil), cliSensitiveHomeEntries...)
	for _, creds := range cliProviderCredentials {
		allSensitive = append(allSensitive, creds...)
	}

	if home != "" && cliPathWithin(home, p) {
		for _, entry := range allSensitive {
			sensitive := filepath.Join(home, filepath.FromSlash(entry))
			if cliPathWithin(p, sensitive) || cliPathWithin(sensitive, p) || p == sensitive {
				return true
			}
		}
		return false
	}
	pSlash := filepath.ToSlash(p)
	for _, entry := range allSensitive {
		entrySlash := "/" + entry
		if strings.Contains(pSlash, entrySlash+"/") || strings.HasSuffix(pSlash, entrySlash) || pSlash == entrySlash {
			return true
		}
		candidate := filepath.Join(p, filepath.FromSlash(entry))
		if fi, err := os.Stat(candidate); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return true
		}
	}
	return false
}

// cliToolchainRoot picks the directory to re-bind for a toolchain binary
// under HOME or masked host roots: the installation root (parent of bin/, sbin/ or shims/)
// when that root holds no sensitive entries, otherwise the containing directory,
// otherwise the binary alone. It never returns HOME or masked roots themselves.
func cliToolchainRoot(home, bin string) string {
	bin = filepath.Clean(bin)
	dir := filepath.Dir(bin)
	switch filepath.Base(dir) {
	case "bin", "sbin", "shims":
		root := filepath.Dir(dir)
		if root != home && !cliIsMaskedHostRoot(root) && !cliPathTouchesSensitive(home, root) {
			return root
		}
	}
	if dir != home && !cliIsMaskedHostRoot(dir) && !cliPathTouchesSensitive(home, dir) {
		return dir
	}
	if !cliPathTouchesSensitive(home, bin) {
		return bin
	}
	return ""
}

// rebindToolchainUnderMaskedRoot ensures that targetBin and any of its symlink hops
// or containing environment directories that reside under masked roots (like /mnt, /home, or /tmp)
// are re-bound read-only so they remain executable inside the sandbox.
// It uses cliToolchainRoot to guarantee that HOME, ~/.local, or any directory containing
// sensitive credentials are NEVER re-bound.
func rebindToolchainUnderMaskedRoot(args []string, targetBin, workspace, home string) []string {
	if targetBin == "" {
		return args
	}
	targetBin = filepath.Clean(targetBin)

	bins := []string{targetBin}
	curr := targetBin
	for i := 0; i < 16; i++ {
		target, err := os.Readlink(curr)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(curr), target)
		}
		target = filepath.Clean(target)
		bins = append(bins, target)
		curr = target
	}

	var toRebind []string
	for _, b := range bins {
		if root := cliToolchainRoot(home, b); root != "" {
			toRebind = append(toRebind, root)
		}
	}

	for _, p := range toRebind {
		if p == "" || p == "/" || p == workspace || cliPathWithin(workspace, p) {
			continue
		}
		underMasked := false
		for _, m := range []string{"/home", "/mnt", "/root", "/media", "/srv"} {
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

	if cliBwrapProbe != nil {
		if probeErr := cliBwrapProbe(bwrapPath); probeErr != nil {
			if isCLISandboxRequired(ctx) {
				return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0,
					probeErr.Error(), probeErr)
			}
			// In local trusted mode without mandatory sandbox requirement, gracefully fallback to unsandboxed execution
			return cmd, nil
		}
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
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run",
	}

	// Mask host roots (/root, /media, /srv, /mnt, /home)
	for _, r := range []string{"/root", "/media", "/srv"} {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() && workspace != r {
			bwrapArgs = append(bwrapArgs, "--tmpfs", r)
		}
	}
	if fi, err := os.Stat("/mnt"); err == nil && fi.IsDir() && workspace != "/mnt" {
		bwrapArgs = append(bwrapArgs, "--tmpfs", "/mnt")
	}
	if fi, err := os.Stat("/home"); err == nil && fi.IsDir() && workspace != "/home" {
		bwrapArgs = append(bwrapArgs, "--tmpfs", "/home")
	}

	// Mask server state directories and config files (/var/lib/makewand, /etc/makewand, MAKEWAND_DATA_DIR, etc.)
	bwrapArgs = cliMaskServerPaths(bwrapArgs, workspace, cmd.Env)

	childEnv := sanitizeCLIEnv(provider, cmd.Env)
	home := cliEnvValue(childEnv, "HOME")
	p := strings.TrimSuffix(strings.ToLower(provider), "-cli")
	var selectedCodexHome, realCodexHome string
	if p == "codex" {
		var err error
		selectedCodexHome, realCodexHome, err = cliConfiguredCodexHome(childEnv, workspace, home)
		if err != nil {
			return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
		}
	}
	var allowedCreds []string
	homeMaskedWithTmpfs := false
	if home != "" {
		home = filepath.Clean(home)
		allowedCreds = cliProviderCredentials[p]
		if selectedCodexHome != "" {
			allowedCreds = nil // Explicit account selection does not expose the default account.
		}

		isRemote := RemoteOriginFromContext(ctx)
		isTempHome := cliPathWithin("/tmp", home) || cliPathWithin(os.TempDir(), home)
		if isRemote || (!isTempHome && home != "/") {
			if workspace != home && !cliPathWithin(workspace, home) {
				bwrapArgs = append(bwrapArgs, "--tmpfs", home)
				homeMaskedWithTmpfs = true
			}
		} else if isTempHome && home != "/tmp" && home != filepath.Clean(os.TempDir()) {
			if workspace != home && !cliPathWithin(workspace, home) {
				bwrapArgs = append(bwrapArgs, "--bind", home, home)
			}
		}
	}

	// Resolve target binary
	targetBin := cmd.Path
	if len(cmd.Args) > 0 && cmd.Args[0] != "" {
		targetBin = cmd.Args[0]
	}
	if !filepath.IsAbs(targetBin) {
		if resolved, err := exec.LookPath(targetBin); err == nil {
			targetBin = resolved
		}
	}

	// In test environments (go test), test binaries, scripts, or fixtures may reside under /tmp.
	// Ensure test roots under /tmp are bound so test fixtures continue to work with --tmpfs /tmp.
	bindTmpTestFixture := func(targetPath string) {
		if !cliPathWithin("/tmp", targetPath) || targetPath == "/tmp" {
			return
		}
		rel, err := filepath.Rel("/tmp", targetPath)
		if err != nil {
			return
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) == 0 {
			return
		}
		top := parts[0]
		fixtureRoot := filepath.Join("/tmp", top)
		if strings.HasPrefix(top, "Test") || strings.HasPrefix(top, "go-build") {
			if _, err := os.Stat(fixtureRoot); err == nil {
				already := false
				for i := 0; i+1 < len(bwrapArgs); i++ {
					if (bwrapArgs[i] == "--bind" || bwrapArgs[i] == "--ro-bind") && bwrapArgs[i+1] == fixtureRoot {
						already = true
						break
					}
				}
				if !already {
					if strings.HasPrefix(top, "go-build") {
						bwrapArgs = append(bwrapArgs, "--ro-bind", fixtureRoot, fixtureRoot)
					} else {
						bwrapArgs = append(bwrapArgs, "--bind", fixtureRoot, fixtureRoot)
					}
				}
			}
		}
	}

	bindTmpTestFixture(targetBin)
	for _, arg := range cmd.Args {
		bindTmpTestFixture(arg)
	}
	for _, envEntry := range cmd.Env {
		if parts := strings.SplitN(envEntry, "=", 2); len(parts) == 2 {
			bindTmpTestFixture(parts[1])
		}
	}

	// Mount workspace (after host roots /tmpfs mounts so it overlays correctly)
	if isReview {
		bwrapArgs = append(bwrapArgs, "--ro-bind", workspace, workspace)
	} else {
		bwrapArgs = append(bwrapArgs, "--bind", workspace, workspace)
	}

	// Re-bind target binary and any symlink targets (essential if binary or its environment is under /tmp, /mnt, or /home)
	bwrapArgs = rebindToolchainUnderMaskedRoot(bwrapArgs, targetBin, workspace, home)

	// Bind active provider credentials within home and mask sensitive credentials
	if home != "" {
		bindFlag := "--bind"
		if isReview || RemoteOriginFromContext(ctx) {
			bindFlag = "--ro-bind"
		}
		for _, cred := range allowedCreds {
			target := filepath.Join(home, filepath.FromSlash(cred))
			if target == workspace || cliPathWithin(target, workspace) {
				continue
			}
			if _, err := os.Stat(target); err == nil { //nolint:gosec // G703: checking existence of provider credentials in host home
				already := false
				for i := 0; i+1 < len(bwrapArgs); i++ {
					if (bwrapArgs[i] == "--bind" || bwrapArgs[i] == "--ro-bind") && bwrapArgs[i+1] == target {
						already = true
						break
					}
				}
				if !already {
					bwrapArgs = append(bwrapArgs, bindFlag, target, target)
				}
			}
		}

		if !homeMaskedWithTmpfs {
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
				if prov == p && selectedCodexHome == "" {
					continue
				}
				for _, c := range creds {
					if !isAllowed(c) {
						toMask = append(toMask, c)
					}
				}
			}

			for _, entry := range toMask {
				authorized, err := cliHomeMaskTarget(home, entry)
				if err != nil {
					return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
				}
				target := authorized.path
				if target == workspace || cliPathWithin(target, workspace) {
					continue
				}
				bwrapArgs, err = maskSensitivePath(bwrapArgs, authorized, workspace)
				if err != nil {
					return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
				}
			}
		}
	}
	if selectedCodexHome != "" {
		// Mount only the selected account after masking host roots and other
		// credentials. Use its canonical path inside the sandbox: a destination
		// symlink may point into a masked root and cannot serve as a mount point.
		bindFlag := "--bind"
		if isReview || RemoteOriginFromContext(ctx) {
			bindFlag = "--ro-bind"
		}
		bwrapArgs = append(bwrapArgs, bindFlag, realCodexHome, realCodexHome,
			"--setenv", "CODEX_HOME", realCodexHome)
	} else if p != "codex" {
		// A custom Codex account may be outside a masked host root. Other
		// providers must not gain access to it through the root read-only mount.
		sourceEnv := cmd.Env
		if sourceEnv == nil {
			sourceEnv = os.Environ()
		}
		_, foreignHome, err := cliConfiguredCodexHome(sourceEnv, workspace, home)
		if err != nil {
			// Without a verified target, the root read-only mount can expose
			// an explicitly selected account to a different provider.
			return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
		}
		if foreignHome != "" {
			bwrapArgs, err = maskSensitivePath(bwrapArgs, cliCredentialMaskTarget{foreignHome}, workspace)
			if err != nil {
				return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
			}
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

	wrapped := exec.CommandContext(ctx, bwrapPath, bwrapArgs...) //nolint:gosec // G702: bwrap invocation wrapping CLI command
	wrapped.Dir = cmd.Dir
	wrapped.Env = childEnv
	wrapped.Stdin = cmd.Stdin
	wrapped.Stdout = cmd.Stdout
	wrapped.Stderr = cmd.Stderr
	wrapped.ExtraFiles = cmd.ExtraFiles

	return wrapped, nil
}

// sanitizeCLIEnv filters out foreign credentials, tokens, and secrets
// while preserving the active provider's specific keys/variables and standard system runtime variables.
func sanitizeCLIEnv(provider string, env []string) []string {
	if env == nil {
		env = os.Environ()
	}

	p := strings.ToLower(provider)
	p = strings.TrimSuffix(p, "-cli")
	if p == "antigravity" {
		p = "agy"
	}

	isProviderKey := func(key string, prov string) bool {
		upperKey := strings.ToUpper(key)
		switch prov {
		case "claude":
			return upperKey == "ANTHROPIC_API_KEY" || upperKey == "CLAUDE_API_KEY" ||
				strings.HasPrefix(upperKey, "ANTHROPIC_") || strings.HasPrefix(upperKey, "CLAUDE_")
		case "codex":
			return upperKey == "OPENAI_API_KEY" || upperKey == "CODEX_API_KEY" || upperKey == "CODEX_HOME" ||
				strings.HasPrefix(upperKey, "OPENAI_") || strings.HasPrefix(upperKey, "CODEX_")
		case "gemini", "agy":
			return upperKey == "GEMINI_API_KEY" || upperKey == "GOOGLE_API_KEY" || upperKey == "GOOGLE_APPLICATION_CREDENTIALS" ||
				strings.HasPrefix(upperKey, "GEMINI_") || strings.HasPrefix(upperKey, "AGY_") || strings.HasPrefix(upperKey, "ANTIGRAVITY_") ||
				strings.HasPrefix(upperKey, "GOOGLE_")
		case "grok":
			return upperKey == "XAI_API_KEY" || upperKey == "GROK_API_KEY" || upperKey == "GROK_AUTH_TOKEN" ||
				upperKey == "GROK_WEB_FETCH_PROXY" || strings.HasPrefix(upperKey, "GROK_") || strings.HasPrefix(upperKey, "XAI_")
		case "muse":
			return upperKey == "META_API_KEY" || strings.HasPrefix(upperKey, "MUSE_") || strings.HasPrefix(upperKey, "META_")
		case "aider":
			return upperKey == "AIDER_API_KEY" || upperKey == "AIDER_MODEL" || upperKey == "OPENAI_API_KEY" ||
				upperKey == "ANTHROPIC_API_KEY" || upperKey == "GEMINI_API_KEY" || upperKey == "DEEPSEEK_API_KEY" ||
				upperKey == "OPENROUTER_API_KEY" || upperKey == "OPENAI_API_BASE" || upperKey == "OLLAMA_API_BASE" ||
				strings.HasPrefix(upperKey, "AIDER_")
		}
		return false
	}

	isOtherProviderKey := func(key string) bool {
		allProviders := []string{"claude", "codex", "gemini", "grok", "muse", "aider"}
		for _, prov := range allProviders {
			if prov == p || (prov == "gemini" && p == "agy") || (prov == "agy" && p == "gemini") {
				continue
			}
			if isProviderKey(key, prov) {
				return true
			}
		}
		upperKey := strings.ToUpper(key)
		for _, prefix := range []string{"DEEPSEEK_", "OPENROUTER_", "MISTRAL_", "COHERE_", "TOGETHER_", "PERPLEXITY_"} {
			if strings.HasPrefix(upperKey, prefix) {
				return true
			}
		}
		return false
	}

	isSecretKey := func(key string) bool {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "MAKEWAND_SERVER_") ||
			upper == "MAKEWAND_AUTH_CONFIG" ||
			upper == "MAKEWAND_DATA_DIR" ||
			upper == "MAKEWAND_CONFIG_DIR" ||
			upper == "MAKEWAND_ADMIN_SESSION_SECRET" ||
			upper == "MAKEWAND_STATE_DB" {
			return true
		}
		if strings.HasPrefix(upper, "AWS_") || strings.HasPrefix(upper, "AZURE_") {
			return true
		}
		for _, t := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_PAT", "GITLAB_TOKEN", "GL_TOKEN", "GITEA_TOKEN"} {
			if upper == t {
				return true
			}
		}
		for _, db := range []string{"DATABASE_URL", "DB_PASSWORD", "DATABASE_PASSWORD", "PGPASSWORD", "MYSQL_PWD", "MONGO_URI", "REDIS_URL", "REDIS_PASSWORD"} {
			if upper == db {
				return true
			}
		}
		for _, s := range []string{"VAULT_TOKEN", "STRIPE_API_KEY", "STRIPE_SECRET_KEY", "SLACK_BOT_TOKEN", "SLACK_TOKEN", "SENTRY_AUTH_TOKEN", "DATADOG_API_KEY", "DD_API_KEY", "NPM_TOKEN"} {
			if upper == s {
				return true
			}
		}
		for _, pattern := range []string{"_SECRET", "_PASSWORD", "_PASSWD", "_TOKEN", "_PRIVATE_KEY"} {
			if strings.Contains(upper, pattern) {
				return true
			}
		}
		return false
	}

	sanitized := make([]string, 0, len(env))
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		key := parts[0]

		if isProviderKey(key, p) {
			sanitized = append(sanitized, entry)
			continue
		}

		if isOtherProviderKey(key) {
			continue
		}

		if isSecretKey(key) {
			continue
		}

		sanitized = append(sanitized, entry)
	}

	return sanitized
}
