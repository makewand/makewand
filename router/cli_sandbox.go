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

func maskSensitivePath(args []string, target, workspace string) []string {
	info, err := os.Lstat(target) //nolint:gosec // G703: sensitive target path is probed on host for sandbox masking
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
		for _, m := range []string{"/home", "/mnt", "/root", "/media", "/srv"} {
			if cliPathWithin(m, real) && !cliPathWithin(workspace, real) {
				return args
			}
		}
		target = real
		info, err = os.Stat(target) //nolint:gosec // G703: real path after symlink evaluation
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
// or containing environment directories that reside under masked roots (like /mnt, /home, or /tmp)
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

	home := os.Getenv("HOME")
	var p string
	var allowedCreds []string
	homeMaskedWithTmpfs := false
	if home != "" {
		home = filepath.Clean(home)
		p = strings.ToLower(provider)
		p = strings.TrimSuffix(p, "-cli")
		allowedCreds = cliProviderCredentials[p]

		if cliPathWithin("/home", home) && home != "/home" {
			if workspace != home && !cliPathWithin(workspace, home) {
				bwrapArgs = append(bwrapArgs, "--tmpfs", home)
				homeMaskedWithTmpfs = true
			}
		} else if cliPathWithin("/tmp", home) && home != "/tmp" {
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
	bwrapArgs = rebindToolchainUnderMaskedRoot(bwrapArgs, targetBin, workspace)

	// Bind active provider credentials within home and mask sensitive credentials
	if home != "" {
		for _, cred := range allowedCreds {
			target := filepath.Join(home, filepath.FromSlash(cred))
			if target == workspace || cliPathWithin(target, workspace) {
				continue
			}
			if _, err := os.Stat(target); err == nil { //nolint:gosec // G703: checking existence of provider credentials in host home
				already := false
				for i := 0; i+1 < len(bwrapArgs); i++ {
					if bwrapArgs[i] == "--bind" && bwrapArgs[i+1] == target {
						already = true
						break
					}
				}
				if !already {
					bwrapArgs = append(bwrapArgs, "--bind", target, target)
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
	wrapped.Env = sanitizeCLIEnv(provider, cmd.Env)
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
