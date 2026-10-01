package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sandbox HOME policy (verification, restricted project plans and preview):
//
//  1. The host HOME is hidden wholesale with a tmpfs. Nothing under it is
//     visible unless it is re-bound explicitly - a deny-by-default layout, so
//     credential stores the list below does not know about stay hidden too.
//  2. Toolchain directories that live under HOME (nvm, a Go SDK in ~/go,
//     rustup, virtualenvs, pipx venvs, ...) are re-bound READ-ONLY. They are
//     resolved from the same PATH the sandbox uses, so the sandbox runs the
//     exact toolchain the host would run instead of silently falling back to
//     an older system binary.
//  3. The workspace is bound read-write last, so it stays writable even when
//     it lives under HOME or under a re-bound toolchain directory.
//
// Only paths that exist are bound, so bubblewrap never has to create mount
// points on the read-only root (which used to fail with "Can't mkdir").

// sandboxSensitiveHomeEntries are HOME-relative paths that must never be
// visible inside a sandbox. HOME is hidden wholesale, so this list is NOT the
// primary control: it narrows toolchain re-binds (a toolchain root that would
// contain or live inside one of these is reduced to its bin directory or to
// the single binary) and drives the per-entry fallback mask used when HOME
// cannot be hidden wholesale (the workspace is HOME itself, or HOME is "/").
var sandboxSensitiveHomeEntries = []string{
	".ssh",
	".aws",
	".azure",
	".gnupg",
	".kube",
	".config",
	".docker",
	".netrc",
	".npmrc",
	".pypirc",
	".pgpass",
	".gitconfig",
	".git-credentials",
	".vault-token",
	".password-store",
	".terraform.d",
	".m2/settings.xml",
	".gradle/gradle.properties",
	".cargo/credentials",
	".cargo/credentials.toml",
	// AI CLI subscription credentials and session state (the providers
	// makewand itself drives).
	".claude",
	".claude.json",
	".codex",
	".gemini",
	".agy",
	".antigravity",
	".grok",
	".aider",
	".local/share/makewand",
	".local/share/keyrings",
	".local/share/fish",
	".local/state",
	// Shell and REPL history.
	".bash_history",
	".zsh_history",
	".histfile",
	".python_history",
	".node_repl_history",
	".psql_history",
	".mysql_history",
	".sqlite_history",
	".lesshst",
	".viminfo",
}

// sandboxToolchainCommands are the executables whose host installation is
// re-bound into the sandbox when it lives under HOME. It covers every command
// the restricted allowlist can run plus the helpers those launch (npx, rustc,
// rustup, gofmt).
var sandboxToolchainCommands = []string{
	"go", "gofmt",
	"node", "npm", "npx", "pnpm", "yarn",
	"python3", "python", "pip", "pip3", "pytest",
	"cargo", "rustc", "rustup",
}

// hostGoEnv is the subset of `go env` the sandbox needs to reproduce the host
// Go toolchain without network access.
type hostGoEnv struct {
	GOROOT     string
	GOMODCACHE string
	GOVERSION  string
	GOPROXY    string
	GOPRIVATE  string
	GONOPROXY  string
	GONOSUMDB  string
	GOSUMDB    string
	GOINSECURE string
}

var (
	hostGoEnvMu    sync.Mutex
	hostGoEnvCache = map[string]hostGoEnv{}
)

// sandboxGoEnvProbe resolves the host Go environment for goBin. Package
// variable so unit tests never exec a real toolchain.
var sandboxGoEnvProbe = probeHostGoEnv

// probeHostGoEnv runs `go env` for the host toolchain. It never runs inside
// the (untrusted) workspace - a go.mod there could request a toolchain switch -
// and pins GOTOOLCHAIN=local so the probe itself can never download anything.
func probeHostGoEnv(goBin string) (hostGoEnv, error) {
	key := goBin + "\x00" + os.Getenv("HOME") + "\x00" + os.Getenv("GOMODCACHE") + "\x00" + os.Getenv("GOPATH") + "\x00" + os.Getenv("GOPROXY") + "\x00" + os.Getenv("GOROOT")
	hostGoEnvMu.Lock()
	if cached, ok := hostGoEnvCache[key]; ok {
		hostGoEnvMu.Unlock()
		return cached, nil
	}
	hostGoEnvMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	//nolint:gosec // G204: goBin is the host toolchain resolved from the user's PATH; fixed argv, no shell.
	cmd := exec.CommandContext(ctx, goBin, "env", "-json",
		"GOROOT", "GOMODCACHE", "GOVERSION", "GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE")
	cmd.Dir = string(filepath.Separator)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		return hostGoEnv{}, fmt.Errorf("go env: %w", err)
	}
	var env hostGoEnv
	if err := json.Unmarshal(out, &env); err != nil {
		return hostGoEnv{}, fmt.Errorf("parse go env: %w", err)
	}
	hostGoEnvMu.Lock()
	hostGoEnvCache[key] = env
	hostGoEnvMu.Unlock()
	return env, nil
}

// sandboxHomeLayout is the HOME-related part of a bubblewrap command line.
type sandboxHomeLayout struct {
	// beforeWorkspace must precede the workspace bind: the HOME tmpfs and the
	// read-only toolchain re-binds.
	beforeWorkspace []string
	// afterWorkspace must follow the workspace bind. It is used when HOME lies
	// inside the workspace (or is the workspace), where hiding HOME first would
	// be undone by the workspace bind.
	afterWorkspace []string
	// env holds extra key/value pairs for the sandbox environment
	// (GOTOOLCHAIN, GOPROXY, RUSTUP_HOME, ...), flattened as k1, v1, k2, v2.
	env []string
	// goVersion is the host Go toolchain version the sandbox exposes
	// ("go1.27.1"), or "" when no Go toolchain was resolved.
	goVersion string
}

// setenvArgs renders the layout environment as bwrap --setenv arguments.
func (l sandboxHomeLayout) setenvArgs() []string {
	out := make([]string, 0, len(l.env)/2*3)
	for i := 0; i+1 < len(l.env); i += 2 {
		out = append(out, "--setenv", l.env[i], l.env[i+1])
	}
	return out
}

// buildSandboxHomeLayout computes how the sandbox presents the host HOME for a
// workspace. home is the host HOME, pathEnv the PATH the sandbox will use.
func buildSandboxHomeLayout(home, workspace, pathEnv string, getenv func(string) string) sandboxHomeLayout {
	var layout sandboxHomeLayout
	home = strings.TrimSpace(home)
	if home == "" || !filepath.IsAbs(home) {
		return layout
	}
	home = filepath.Clean(home)
	workspace = filepath.Clean(workspace)
	if getenv == nil {
		getenv = os.Getenv
	}

	roots, env, goVersion := resolveSandboxToolchains(home, workspace, pathEnv, getenv)
	layout.env = env
	layout.goVersion = goVersion

	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		// Nothing to hide (and nothing could have been installed under it).
		return layout
	}

	switch {
	case home == string(filepath.Separator) || workspace == home:
		// HOME cannot be hidden wholesale without hiding the root filesystem or
		// the workspace itself: mask the known-sensitive entries individually,
		// after the workspace bind so the bind cannot re-expose them.
		layout.afterWorkspace = sandboxMaskSensitiveEntries(home, workspace)
	case pathWithin(workspace, home):
		// HOME lives inside the workspace: hide it after the workspace bind.
		layout.afterWorkspace = sandboxHideHomeArgs(home, roots)
	default:
		layout.beforeWorkspace = sandboxHideHomeArgs(home, roots)
	}
	return layout
}

func sandboxHideHomeArgs(home string, roots []string) []string {
	out := []string{"--tmpfs", home}
	for _, root := range roots {
		out = append(out, "--ro-bind", root, root)
	}
	return out
}

// sandboxMaskSensitiveEntries masks each existing sensitive HOME entry in
// place: directories get an empty tmpfs, files (or symlinks to files) are
// shadowed by /dev/null. Missing entries are skipped, so no mount point ever
// has to be created. Entries containing keep (the workspace) are left alone.
func sandboxMaskSensitiveEntries(home, keep string) []string {
	var out []string
	for _, entry := range sandboxSensitiveHomeEntries {
		target := filepath.Join(home, filepath.FromSlash(entry))
		if pathWithin(target, keep) {
			continue
		}
		info, err := os.Lstat(target)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// bwrap follows the destination symlink; mask whatever it resolves to.
			resolved, statErr := os.Stat(target)
			if statErr != nil {
				continue // dangling: nothing readable behind it
			}
			info = resolved
		}
		if info.IsDir() {
			out = append(out, "--tmpfs", target)
		} else {
			out = append(out, "--ro-bind", os.DevNull, target)
		}
	}
	return out
}

// sandboxMaskedHostRoots lists host root prefixes that should be masked with tmpfs
// to prevent cross-project and credential inspection.
var sandboxMaskedHostRoots = []string{"/root", "/mnt", "/media", "/srv"}

func sandboxMaskedRoots(workspace string) []string {
	var out []string
	cleanWS := filepath.Clean(workspace)
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home != "" {
		home = filepath.Clean(home)
	}
	for _, r := range sandboxMaskedHostRoots {
		r = filepath.Clean(r)
		if r == "/" || r == cleanWS || (home != "" && r == home) {
			continue
		}
		if pathWithin(r, cleanWS) || (home != "" && pathWithin(r, home)) {
			continue
		}
		fi, err := os.Stat(r)
		if err == nil && fi.IsDir() {
			out = append(out, "--tmpfs", r)
		}
	}
	return out
}

// sandboxWorkspaceSocketMasks scans the workspace for pre-existing AF_UNIX domain sockets
// and masks them with /dev/null so sandboxed commands cannot connect to host daemons.
// Note: It intentionally does NOT skip ignored directories (like .git, node_modules, .cache),
// because sockets placed in those subdirectories are still connectable from the sandbox.
func sandboxWorkspaceSocketMasks(workspace string) []string {
	var out []string
	cleanWS := filepath.Clean(workspace)
	count := 0
	_ = filepath.WalkDir(cleanWS, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if count >= 512 {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			out = append(out, "--ro-bind", os.DevNull, path)
			count++
		}
		return nil
	})
	return out
}

// resolveSandboxToolchains resolves the host toolchains the sandbox needs:
// the read-only HOME subtrees to re-bind, extra environment, and the host Go
// version.
func resolveSandboxToolchains(home, workspace, pathEnv string, getenv func(string) string) ([]string, []string, string) {
	candidates := map[string]struct{}{}
	addRoot := func(root string) {
		root = filepath.Clean(root)
		if root == home || !pathWithin(home, root) || pathWithin(workspace, root) {
			return
		}
		if sandboxPathTouchesSensitive(home, root) {
			return
		}
		if _, err := os.Stat(root); err != nil {
			return
		}
		candidates[root] = struct{}{}
	}

	resolved := map[string]string{}
	for _, name := range sandboxToolchainCommands {
		bin := lookPathIn(name, pathEnv)
		if bin == "" {
			continue
		}
		resolved[name] = bin
		for _, hop := range symlinkChain(bin) {
			if !pathWithin(home, hop) {
				continue
			}
			if root := sandboxToolchainRoot(home, hop); root != "" {
				addRoot(root)
			}
		}
	}

	var env []string
	goVersion := ""
	if goBin := resolved["go"]; goBin != "" {
		// Never let the sandboxed go command download a different toolchain
		// (it has no network for tests anyway); the go.mod requirement is
		// checked against the host version before running instead.
		env = append(env, "GOTOOLCHAIN", "local")
		// Module caches written inside the workspace sandbox HOME stay
		// removable (the default read-only cache breaks clone cleanup).
		env = append(env, "GOFLAGS", "-modcacherw")
		if goroot := strings.TrimSpace(getenv("GOROOT")); goroot != "" {
			env = append(env, "GOROOT", goroot)
		}
		if hostEnv, err := sandboxGoEnvProbe(goBin); err == nil {
			goVersion = strings.TrimSpace(hostEnv.GOVERSION)
			if goroot := strings.TrimSpace(hostEnv.GOROOT); goroot != "" && filepath.IsAbs(goroot) && pathWithin(home, goroot) {
				addRoot(goroot)
			}
			var proxies []string
			if modcache := strings.TrimSpace(hostEnv.GOMODCACHE); modcache != "" && filepath.IsAbs(modcache) {
				download := filepath.Join(modcache, "cache", "download")
				if info, err := os.Stat(download); err == nil && info.IsDir() {
					// The host module cache doubles as a read-only offline
					// proxy, so network-isolated test runs reuse modules the
					// host already has instead of failing to download them.
					if pathWithin(home, download) {
						addRoot(download)
					}
					proxies = append(proxies, "file://"+filepath.ToSlash(download))
				}
			}
			upstream := sanitizeGoProxyList(hostEnv.GOPROXY)
			if upstream == "" {
				upstream = "https://proxy.golang.org,direct"
			}
			proxies = append(proxies, upstream)
			env = append(env, "GOPROXY", strings.Join(proxies, ","))
			for _, kv := range [][2]string{
				{"GOPRIVATE", hostEnv.GOPRIVATE},
				{"GONOPROXY", hostEnv.GONOPROXY},
				{"GONOSUMDB", hostEnv.GONOSUMDB},
				{"GOSUMDB", hostEnv.GOSUMDB},
				{"GOINSECURE", hostEnv.GOINSECURE},
			} {
				if v := strings.TrimSpace(kv[1]); v != "" && !strings.Contains(v, "@") {
					env = append(env, kv[0], v)
				}
			}
		}
	}

	if resolved["cargo"] != "" || resolved["rustc"] != "" || resolved["rustup"] != "" {
		rustupHome := strings.TrimSpace(getenv("RUSTUP_HOME"))
		if rustupHome == "" {
			rustupHome = filepath.Join(home, ".rustup")
		}
		if info, err := os.Stat(rustupHome); err == nil && info.IsDir() {
			if pathWithin(home, rustupHome) {
				addRoot(rustupHome)
			}
			// The sandbox HOME differs from the host one, so rustup proxies
			// need the toolchain location spelled out.
			env = append(env, "RUSTUP_HOME", rustupHome)
		}
	}

	return pruneNestedRoots(candidates), env, goVersion
}

// sandboxToolchainRoot picks the directory to re-bind for a toolchain binary
// under HOME: the installation root (parent of bin/ or shims/) when that root
// holds no sensitive entries, otherwise the containing directory, otherwise
// the binary alone. It never returns HOME itself.
func sandboxToolchainRoot(home, bin string) string {
	dir := filepath.Dir(bin)
	switch filepath.Base(dir) {
	case "bin", "sbin", "shims":
		root := filepath.Dir(dir)
		if root != home && pathWithin(home, root) && !sandboxPathTouchesSensitive(home, root) {
			return root
		}
	}
	if dir != home && pathWithin(home, dir) && !sandboxPathTouchesSensitive(home, dir) {
		return dir
	}
	if !sandboxPathTouchesSensitive(home, bin) {
		return bin
	}
	return ""
}

// sandboxPathTouchesSensitive reports whether exposing p would expose a
// sensitive HOME entry: p contains one, or lies inside one.
func sandboxPathTouchesSensitive(home, p string) bool {
	for _, entry := range sandboxSensitiveHomeEntries {
		sensitive := filepath.Join(home, filepath.FromSlash(entry))
		if pathWithin(p, sensitive) || pathWithin(sensitive, p) {
			return true
		}
	}
	return false
}

// pruneNestedRoots returns the sorted roots, dropping any root nested inside
// another one (the outer bind already exposes it).
func pruneNestedRoots(set map[string]struct{}) []string {
	roots := make([]string, 0, len(set))
	for root := range set {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	var out []string
	for _, root := range roots {
		nested := false
		for _, kept := range out {
			if pathWithin(kept, root) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, root)
		}
	}
	return out
}

// lookPathIn resolves name against an explicit PATH value, like exec.LookPath
// but without consulting the process environment. Relative PATH entries are
// ignored: inside the sandbox they would resolve against the workspace.
func lookPathIn(name, pathEnv string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}

// symlinkChain returns p followed by every symlink hop it resolves through,
// ending with the fully resolved path. Each hop must be visible inside the
// sandbox for the lookup to succeed there.
func symlinkChain(p string) []string {
	chain := []string{filepath.Clean(p)}
	cur := filepath.Clean(p)
	for i := 0; i < 40; i++ {
		info, err := os.Lstat(cur)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(cur)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		cur = filepath.Clean(target)
		chain = append(chain, cur)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil && real != cur {
		chain = append(chain, real)
	}
	return chain
}

// sanitizeGoProxyList drops proxy entries that embed credentials: the sandbox
// must not receive secrets, and GOPROXY URLs can carry userinfo. Each kept
// entry keeps the separator (',' or '|') that originally followed it.
func sanitizeGoProxyList(value string) string {
	var b strings.Builder
	sep := ""
	start := 0
	value = strings.TrimSpace(value)
	for i := 0; i <= len(value); i++ {
		if i < len(value) && value[i] != ',' && value[i] != '|' {
			continue
		}
		part := strings.TrimSpace(value[start:i])
		start = i + 1
		if part == "" || strings.Contains(part, "@") {
			continue
		}
		b.WriteString(sep)
		b.WriteString(part)
		sep = ","
		if i < len(value) {
			sep = string(value[i])
		}
	}
	return b.String()
}

// goModRequiredVersion returns the `go` directive of the workspace go.mod as a
// Go version string ("go1.26"), or "" when there is none.
func goModRequiredVersion(workspace string) string {
	data, err := os.ReadFile(filepath.Join(workspace, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			v := "go" + fields[1]
			if version.IsValid(v) {
				return v
			}
			return ""
		}
	}
	return ""
}

// checkSandboxGoToolchain fails early, with an environment (not candidate)
// error, when the workspace requires a newer Go than the host toolchain the
// sandbox exposes. The sandbox pins GOTOOLCHAIN=local and has no network for
// tests, so the go command could never satisfy the requirement by itself.
func checkSandboxGoToolchain(workspace, hostVersion string) error {
	required := goModRequiredVersion(workspace)
	hostVersion = strings.TrimSpace(hostVersion)
	if required == "" || hostVersion == "" || !version.IsValid(hostVersion) {
		return nil
	}
	if version.Compare(hostVersion, required) >= 0 {
		return nil
	}
	return fmt.Errorf("go.mod requires %s but the host Go toolchain is %s; the verification sandbox uses the host toolchain (GOTOOLCHAIN=local, no downloads), so install Go %s or newer", required, hostVersion, strings.TrimPrefix(required, "go"))
}
