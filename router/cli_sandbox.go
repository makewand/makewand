package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/internal/seccompbpf"
	"github.com/makewand/makewand/internal/trustedbin"
)

var (
	cliBwrapLookup = trustedbin.LookPath
	cliBwrapProbe  = defaultCLIBwrapProbe
)

// cliTestExtraBinds lets _test.go files add bubblewrap mounts for go test
// fixtures (the test binary and t.TempDir roots under the sandbox's /tmp
// tmpfs). It is nil in production: no code outside _test.go files sets it.
var cliTestExtraBinds func(targetBin string, env []string) []string

// cliTestEnvAllowed lets _test.go files pass fixture-control variables
// through sanitizeCLIEnv. It is nil in production.
var cliTestEnvAllowed func(key string) bool

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
	".yarnrc",
	".pypirc",
	".pgpass",
	".gitconfig",
	".git-credentials",
	".cargo/credentials",
	".cargo/credentials.toml",
	".cargo/config.toml",
	".m2/settings.xml",
	".gradle/gradle.properties",
	".s3cfg",
	".boto",
	".oci",
	".aider.conf.yml",
	".anthropic",
	".openai",
	".vault-token",
	".password-store",
	".terraform.d",
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
	".local/share/keyrings",
	".local/share/muse",
	".local/share/credentials",
	".local/share/makewand",
	".local/share/fish",
	".local/state",
	".mozilla",
	".pki",
	"server_auth.json",
	"state.db",
	"state.db-wal",
	"state.db-shm",
	"admin_session_secret",
	"users.json",
	"audit.jsonl",
	"usage.jsonl",
	".config/makewand",
	".config/gcloud",
	".config/gh",
	".config/hub",
	".config/git/credentials",
	".config/rclone",
	".config/muse",
	".config/google-chrome",
	".config/chromium",
}

// cliForbiddenWritableRoots defines system and sensitive roots that must never
// be used as a sandbox workspace (VUL-01/02/06). Mirrors the Python sandbox's
// FORBIDDEN_WRITABLE_ROOTS plus its _broad_workspace roots (/var/tmp) and the
// masked host roots (/mnt, /srv, /media), whose masks a workspace bind would undo.
var cliForbiddenWritableRoots = []string{
	"/", "/tmp", "/var/tmp", "/home", "/etc", "/usr", "/var", "/root", "/boot", "/opt",
	"/sys", "/dev", "/proc", "/mnt", "/srv", "/media",
}

// cliCleanAndReal returns the cleaned path and, when it resolves, its
// symlink-free form.
func cliCleanAndReal(p string) []string {
	clean := filepath.Clean(p)
	out := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		if real := filepath.Clean(resolved); real != clean {
			out = append(out, real)
		}
	}
	return out
}

// isForbiddenWritableWorkspace reports whether workspace is a protected root,
// HOME itself, or an ancestor of HOME. HOME is taken from the process
// environment, os.UserHomeDir, and any extraHomes (the child's HOME); every
// comparison is repeated after symlink resolution.
func isForbiddenWritableWorkspace(workspace string, extraHomes ...string) bool {
	wsForms := cliCleanAndReal(workspace)
	for _, root := range cliForbiddenWritableRoots {
		for _, rootForm := range cliCleanAndReal(root) {
			for _, wsForm := range wsForms {
				if wsForm == rootForm {
					return true
				}
			}
		}
	}
	homes := append([]string{os.Getenv("HOME")}, extraHomes...)
	if h, err := os.UserHomeDir(); err == nil {
		homes = append(homes, h)
	}
	for _, home := range homes {
		if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) {
			continue
		}
		for _, homeForm := range cliCleanAndReal(home) {
			for _, wsForm := range wsForms {
				// HOME itself or any ancestor of HOME would expose every
				// credential directory the sandbox masks.
				if cliPathWithin(wsForm, homeForm) {
					return true
				}
			}
		}
	}
	return false
}

// generateSeccompBPFFilter returns the shared seccomp filter (see
// internal/seccompbpf), nil on unsupported architectures.
func generateSeccompBPFFilter() []byte {
	return seccompbpf.GenerateFilter()
}

var cliProviderCredentials = map[string][]string{
	"claude":      {".claude", ".claude.json", ".claude-2", ".claude-3"},
	"codex":       {".codex", ".codex-2", ".codex-3"},
	"gemini":      {".gemini", ".agy", ".antigravity"},
	"agy":         {".gemini", ".agy", ".antigravity"},
	"antigravity": {".gemini", ".agy", ".antigravity"},
	"grok":        {".grok"},
	"aider":       {".aider"},
}

type cliProtectedSubpath struct {
	subpath     string
	isDir       bool
	placeholder string // if non-empty, default placeholder content or "dir"
}

var cliProviderProtected = map[string][]cliProtectedSubpath{
	"claude": {
		{subpath: "CLAUDE.md", isDir: false, placeholder: ""},
		{subpath: "settings.json", isDir: false, placeholder: "{}\n"},
		{subpath: "settings.local.json", isDir: false, placeholder: "{}\n"},
		{subpath: "commands", isDir: true, placeholder: "dir"},
		{subpath: "agents", isDir: true, placeholder: "dir"},
		{subpath: "skills", isDir: true, placeholder: "dir"},
		{subpath: "plugins", isDir: true, placeholder: "dir"},
		{subpath: "hooks", isDir: true, placeholder: "dir"},
		{subpath: "output-styles", isDir: true, placeholder: "dir"},
		{subpath: "rules", isDir: true, placeholder: "dir"},
		{subpath: "workflows", isDir: true, placeholder: "dir"},
		{subpath: "themes", isDir: true, placeholder: ""},
		{subpath: "keybindings.json", isDir: false, placeholder: ""},
		{subpath: "remote-settings.json", isDir: false, placeholder: ""},
		{subpath: "policy-limits.json", isDir: false, placeholder: ""},
		{subpath: "chrome", isDir: true, placeholder: "dir"},
		{subpath: "local", isDir: true, placeholder: ""},
	},
	"codex": {
		{subpath: "config.toml", isDir: false, placeholder: ""},
		{subpath: "AGENTS.md", isDir: false, placeholder: ""},
		{subpath: "prompts", isDir: true, placeholder: "dir"},
		{subpath: "skills", isDir: true, placeholder: "dir"},
		{subpath: "rules", isDir: true, placeholder: "dir"},
		{subpath: "hooks", isDir: true, placeholder: "dir"},
		{subpath: "AGENTS.override.md", isDir: false, placeholder: ""},
		{subpath: "hooks.json", isDir: false, placeholder: ""},
		{subpath: "policy", isDir: true, placeholder: ""},
		{subpath: "plugins", isDir: true, placeholder: ""},
		{subpath: "packages", isDir: true, placeholder: ""},
		{subpath: "memories", isDir: true, placeholder: ""},
	},
	"gemini": {
		{subpath: "settings.json", isDir: false, placeholder: "{}\n"},
		{subpath: "GEMINI.md", isDir: false, placeholder: ""},
		{subpath: "commands", isDir: true, placeholder: "dir"},
		{subpath: "extensions", isDir: true, placeholder: "dir"},
		{subpath: "trustedFolders.json", isDir: false, placeholder: ""},
		{subpath: "policies", isDir: true, placeholder: ""},
		{subpath: "skills", isDir: true, placeholder: ""},
		{subpath: "config", isDir: true, placeholder: ""},
		{subpath: "antigravity/mcp_config.json", isDir: false, placeholder: ""},
		{subpath: "antigravity/browserAllowlist.txt", isDir: false, placeholder: ""},
		{subpath: "antigravity/user_settings.pb", isDir: false, placeholder: ""},
		{subpath: "antigravity-cli/bin", isDir: true, placeholder: ""},
		{subpath: "antigravity-cli/builtin", isDir: true, placeholder: ""},
		{subpath: "antigravity-cli/updater", isDir: true, placeholder: ""},
		{subpath: "antigravity-cli/knowledge", isDir: true, placeholder: ""},
		{subpath: "antigravity-cli/hooks.json", isDir: false, placeholder: ""},
		{subpath: "antigravity-cli/settings.json", isDir: false, placeholder: ""},
		{subpath: "antigravity-cli/mcp_config.json", isDir: false, placeholder: ""},
	},
	"grok": {
		{subpath: "config.toml", isDir: false, placeholder: ""},
		{subpath: "bin", isDir: true, placeholder: ""},
		{subpath: "hooks", isDir: true, placeholder: "dir"},
		{subpath: "hooks-paths", isDir: false, placeholder: ""},
		{subpath: "installed-plugins", isDir: true, placeholder: ""},
		{subpath: "bundled", isDir: true, placeholder: ""},
		{subpath: "vendor", isDir: true, placeholder: ""},
		{subpath: "completions", isDir: true, placeholder: ""},
		{subpath: "marketplace-cache", isDir: true, placeholder: ""},
		{subpath: "memory-v2", isDir: true, placeholder: ""},
		{subpath: "sandbox.toml", isDir: false, placeholder: ""},
		{subpath: "trusted_folders.toml", isDir: false, placeholder: ""},
	},
	"muse": {
		{subpath: "settings.json", isDir: false, placeholder: ""},
		{subpath: "env", isDir: false, placeholder: ""},
		{subpath: "trust.json", isDir: false, placeholder: ""},
		{subpath: "plugins", isDir: true, placeholder: ""},
		{subpath: "skills", isDir: true, placeholder: ""},
		{subpath: "feature-config", isDir: true, placeholder: ""},
	},
}

var cliProviderEphemeral = map[string][]string{
	"claude": {
		"shell-snapshots", "session-env", "sessions", "ide", "daemon", "jobs", "bridge-spawn",
	},
	"codex": {
		"shell_snapshots", "app-server-control", "app-server-daemon",
	},
	"muse": {
		"runtime",
	},
}

// UnsafeHostExecAuth carries authorization for running unsandboxed on the host
// when bubblewrap is unavailable.
type UnsafeHostExecAuth struct {
	Acknowledged bool
	Source       string
	Audit        func(command string, args []string, dir string)
}

type unsafeHostExecAuthKey struct{}

// ContextWithUnsafeHostExecAuth returns a new context carrying UnsafeHostExecAuth.
func ContextWithUnsafeHostExecAuth(ctx context.Context, auth UnsafeHostExecAuth) context.Context {
	return context.WithValue(ctx, unsafeHostExecAuthKey{}, auth)
}

// UnsafeHostExecAuthFromContext retrieves the UnsafeHostExecAuth carried in ctx, if any.
func UnsafeHostExecAuthFromContext(ctx context.Context) (UnsafeHostExecAuth, bool) {
	if ctx == nil {
		return UnsafeHostExecAuth{}, false
	}
	auth, ok := ctx.Value(unsafeHostExecAuthKey{}).(UnsafeHostExecAuth)
	return auth, ok
}

// CLIExecRecord records whether an invocation was sandboxed or executed on the host.
type CLIExecRecord struct {
	Sandboxed  bool
	UnsafeHost bool
	Provider   string
	Workspace  string
}

type cliExecObserverKey struct{}

// CLIExecObserver observes whether a CLI invocation ran sandboxed or unsandboxed.
type CLIExecObserver struct {
	mu     sync.Mutex
	record CLIExecRecord
}

func (o *CLIExecObserver) Record() CLIExecRecord {
	if o == nil {
		return CLIExecRecord{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.record
}

func (o *CLIExecObserver) set(r CLIExecRecord) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record = r
}

// ContextWithCLIExecObserver attaches a CLIExecObserver to ctx.
func ContextWithCLIExecObserver(ctx context.Context, obs *CLIExecObserver) context.Context {
	return context.WithValue(ctx, cliExecObserverKey{}, obs)
}

func recordCLIExec(ctx context.Context, r CLIExecRecord) {
	if ctx == nil {
		return
	}
	if obs, ok := ctx.Value(cliExecObserverKey{}).(*CLIExecObserver); ok && obs != nil {
		obs.set(r)
	}
}

// isUnsafeHostExecAuthorized checks whether the caller has explicitly requested
// and completed the one-time acknowledgment for MAKEWAND_UNSAFE_HOST_EXEC.
// Remote origin contexts NEVER allow host execution (fail closed).
func isUnsafeHostExecAuthorized(ctx context.Context) (bool, UnsafeHostExecAuth) {
	if RemoteOriginFromContext(ctx) {
		return false, UnsafeHostExecAuth{}
	}
	if os.Getenv("MAKEWAND_UNSAFE_HOST_EXEC") != "1" {
		return false, UnsafeHostExecAuth{}
	}
	if auth, ok := UnsafeHostExecAuthFromContext(ctx); ok && auth.Acknowledged {
		return true, auth
	}
	if valid, auth := checkDiskConfigHostExecAck(); valid {
		return true, auth
	}
	return false, UnsafeHostExecAuth{}
}

func checkDiskConfigHostExecAck() (bool, UnsafeHostExecAuth) {
	configDir := os.Getenv("MAKEWAND_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false, UnsafeHostExecAuth{}
		}
		configDir = filepath.Join(home, ".config", "makewand")
	}
	cfgFile := filepath.Join(configDir, "config.json")
	// #nosec G304, G703 -- configDir is operator/user local configuration, never an HTTP path or user identifier.
	//nolint:gosec // G304, G703: configDir is operator/user local configuration, not request input.
	data, err := os.ReadFile(cfgFile)
	if err != nil {
		return false, UnsafeHostExecAuth{}
	}
	var cfg struct {
		UnsafeHostExecAckVersion int    `json:"unsafe_host_exec_ack_version"`
		UnsafeHostExecAckHost    string `json:"unsafe_host_exec_ack_host"`
		UnsafeHostExecAckAt      string `json:"unsafe_host_exec_ack_at"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, UnsafeHostExecAuth{}
	}
	if cfg.UnsafeHostExecAckVersion < 1 || strings.TrimSpace(cfg.UnsafeHostExecAckHost) == "" {
		return false, UnsafeHostExecAuth{}
	}
	host, err := os.Hostname()
	if err != nil || host != cfg.UnsafeHostExecAckHost {
		return false, UnsafeHostExecAuth{}
	}
	return true, UnsafeHostExecAuth{
		Acknowledged: true,
		Source:       "config-ack",
		Audit:        auditHostExecToConfigDir(configDir),
	}
}

func auditHostExecToConfigDir(configDir string) func(command string, args []string, dir string) {
	return func(command string, args []string, dir string) {
		entry := struct {
			Time    string   `json:"time"`
			Context string   `json:"context"`
			Command string   `json:"command"`
			Args    []string `json:"args,omitempty"`
			Dir     string   `json:"dir"`
			Source  string   `json:"source"`
		}{
			Time:    time.Now().UTC().Format(time.RFC3339),
			Context: "cli-provider",
			Command: command,
			Args:    args,
			Dir:     dir,
			Source:  "config-ack",
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return
		}
		auditPath := filepath.Join(configDir, "unsafe_exec_audit.jsonl")
		// #nosec G304, G703 -- configDir is operator/user local configuration, never an HTTP path or user identifier.
		//nolint:gosec // G304, G703: configDir is operator/user local configuration, not request input.
		f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		_, _ = f.Write(append(data, '\n'))
		_ = f.Close()
	}
}

// isCLISandboxRequired reports whether execution MUST be sandboxed. Every
// launch requires the sandbox unless unsafe host execution has been explicitly
// authorized (MAKEWAND_UNSAFE_HOST_EXEC=1 plus acknowledgment); the task type
// never relaxes it, because analysis and review prompts carry the same
// untrusted repository content as writing tasks. Remote, restricted, and
// MAKEWAND_REQUIRE_BWRAP contexts require the sandbox even when authorized.
func isCLISandboxRequired(ctx context.Context) bool {
	if RemoteOriginFromContext(ctx) {
		return true
	}
	if os.Getenv("MAKEWAND_RESTRICTED") == "1" || os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
		return true
	}
	authorized, _ := isUnsafeHostExecAuthorized(ctx)
	return !authorized
}

// cliAuditHostExec records an unsandboxed host launch. Authorized callers
// supply an audit sink; without one the launch is still reported on stderr so
// no host execution goes unrecorded.
func cliAuditHostExec(auth UnsafeHostExecAuth, provider string, cmd *exec.Cmd) {
	cmdPath := cmd.Path
	if cmdPath == "" && len(cmd.Args) > 0 {
		cmdPath = cmd.Args[0]
	}
	if auth.Audit != nil {
		auth.Audit(cmdPath, cmd.Args, cmd.Dir)
		return
	}
	fmt.Fprintf(os.Stderr, "makewand: WARNING: running %s CLI unsandboxed on the host (source=%s, command=%s, dir=%s)\n",
		provider, auth.Source, cmdPath, cmd.Dir)
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
	for _, pfx := range []string{".codex", ".claude", ".gemini", ".openai", ".anthropic"} {
		if strings.HasPrefix(entry, pfx) {
			allowed = true
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
	// /tmp is a fresh tmpfs inside the sandbox. A provider binary (or symlink
	// target) living below it is re-exposed read-only together with its own
	// directory (side files such as bundled resources), never writable, and
	// never /tmp itself or a world-writable directory.
	for _, b := range bins {
		dir := filepath.Dir(b)
		if b == workspace || cliPathWithin(workspace, b) || !cliPathWithin("/tmp", dir) || dir == "/tmp" {
			continue
		}
		fi, err := os.Lstat(b)
		di, derr := os.Stat(dir)
		if err != nil || derr != nil || !fi.Mode().IsRegular() || di.Mode().Perm()&0o002 != 0 {
			continue
		}
		args = append(args, "--ro-bind", dir, dir)
	}
	return args
}

// cliShieldProviderState overlays read-only binds for agent instructions, MCP configs,
// hooks, skills, and settings, and mounts ephemeral session directories with tmpfs.
// This prevents prompt-injection attacks from modifying host configurations or planting
// malicious hooks for host RCE (remediating [P1] redteam-1).
//
// In private mode rootPath is the sandbox path of a per-run private provider
// directory: existing host entries are bound read-only onto their place in it
// (a symlinked entry's target is bound at the entry's path), and nothing is
// created on the host because writes already land in the discarded private copy.
func cliShieldProviderState(args []string, providerKey, rootPath, workspace string, isReadOnly, private bool) []string {
	cleanRoot := filepath.Clean(rootPath)
	if cleanRoot == "" || cleanRoot == "." || cleanRoot == "/" || strings.Contains(cleanRoot, "..") {
		return args
	}
	rootPrefix := cleanRoot + string(os.PathSeparator)

	normKey := strings.ToLower(providerKey)
	if normKey == "agy" || normKey == "antigravity" {
		normKey = "gemini"
	}

	if isReadOnly {
		if normKey == "codex" {
			// In tmpfs readonly mode for codex: ro-bind essential config and protected paths into the tmpfs
			for _, sub := range []string{"auth.json", "config.toml", "version.json", "models_cache.json"} {
				subPath := filepath.Clean(filepath.Join(cleanRoot, sub))
				if _, err := os.Stat(subPath); err == nil {
					args = cliAppendRoBindIfNotPresent(args, subPath)
				}
			}
			for _, entry := range cliProviderProtected["codex"] {
				rel := filepath.FromSlash(entry.subpath)
				if !filepath.IsLocal(rel) || strings.Contains(rel, "..") {
					continue
				}
				subPath := filepath.Clean(filepath.Join(cleanRoot, rel))
				if strings.Contains(subPath, "..") || !strings.HasPrefix(subPath, rootPrefix) {
					continue
				}
				if fi, err := os.Lstat(subPath); err == nil {
					if fi.Mode()&os.ModeSymlink != 0 {
						if target, err := filepath.EvalSymlinks(subPath); err == nil {
							args = cliAppendRoBindIfNotPresent(args, target)
						}
					} else {
						args = cliAppendRoBindIfNotPresent(args, subPath)
					}
				}
			}
		}
		return args
	}

	protectedList := cliProviderProtected[normKey]
	for _, entry := range protectedList {
		rel := filepath.FromSlash(entry.subpath)
		if !filepath.IsLocal(rel) || strings.Contains(rel, "..") {
			continue
		}
		subPath := filepath.Clean(filepath.Join(cleanRoot, rel))
		if strings.Contains(subPath, "..") || !strings.HasPrefix(subPath, rootPrefix) {
			continue
		}
		fi, err := os.Lstat(subPath)
		if err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				target, err := filepath.EvalSymlinks(subPath)
				if err == nil {
					if private {
						args = cliAppendRoBindToIfNotPresent(args, target, subPath)
					} else {
						args = cliAppendRoBindIfNotPresent(args, target)
					}
				}
			} else {
				args = cliAppendRoBindIfNotPresent(args, subPath)
			}
		} else if entry.placeholder != "" && !private {
			if entry.isDir {
				// #nosec G301 -- directory placeholder for provider protection inside host home
				if err := os.MkdirAll(subPath, 0o700); err == nil {
					args = cliAppendRoBindIfNotPresent(args, subPath)
				}
			} else {
				dir := filepath.Clean(filepath.Dir(subPath))
				if strings.Contains(dir, "..") || !strings.HasPrefix(dir, rootPrefix) {
					continue
				}
				// #nosec G301 -- directory placeholder for provider protection inside host home
				if err := os.MkdirAll(dir, 0o700); err == nil {
					// #nosec G304, G703 -- fixed subpath under user's provider state directory
					//nolint:gosec // G304, G703: fixed subpath under user's provider state directory
					if err := os.WriteFile(subPath, []byte(entry.placeholder), 0o600); err == nil {
						args = cliAppendRoBindIfNotPresent(args, subPath)
					}
				}
			}
		}
	}

	if normKey == "claude" && workspace != "" {
		projectsDir := filepath.Clean(filepath.Join(cleanRoot, "projects"))
		if !strings.Contains(projectsDir, "..") && strings.HasPrefix(projectsDir, rootPrefix) {
			var sb strings.Builder
			for _, r := range filepath.Clean(workspace) {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
					sb.WriteRune(r)
				} else {
					sb.WriteByte('-')
				}
			}
			key := sb.String()
			if key != "" && !strings.Contains(key, "..") {
				memDir := filepath.Clean(filepath.Join(projectsDir, key, "memory"))
				if !strings.Contains(memDir, "..") && strings.HasPrefix(memDir, rootPrefix) {
					if fi, err := os.Lstat(memDir); err == nil && fi.IsDir() {
						args = cliAppendRoBindIfNotPresent(args, memDir)
					}
				}
			}
		}
	}

	if private {
		// Session state already lands in the discarded private copy.
		return args
	}

	ephemeralList := cliProviderEphemeral[normKey]
	for _, eph := range ephemeralList {
		rel := filepath.FromSlash(eph)
		if !filepath.IsLocal(rel) || strings.Contains(rel, "..") {
			continue
		}
		subPath := filepath.Clean(filepath.Join(cleanRoot, rel))
		if strings.Contains(subPath, "..") || !strings.HasPrefix(subPath, rootPrefix) {
			continue
		}
		fi, err := os.Lstat(subPath)
		if err == nil && fi.IsDir() {
			args = cliAppendTmpfsIfNotPresent(args, subPath)
		}
	}

	return args
}

func cliAppendRoBindIfNotPresent(args []string, target string) []string {
	target = filepath.Clean(target)
	for i := 0; i+1 < len(args); i++ {
		if (args[i] == "--ro-bind" || args[i] == "--bind" || args[i] == "--tmpfs") && filepath.Clean(args[i+1]) == target {
			if args[i] == "--ro-bind" {
				return args
			}
		}
	}
	return append(args, "--ro-bind", target, target)
}

// cliAppendRoBindToIfNotPresent read-only binds source at dest unless dest
// already has a read-only bind.
func cliAppendRoBindToIfNotPresent(args []string, source, dest string) []string {
	dest = filepath.Clean(dest)
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && filepath.Clean(args[i+2]) == dest {
			return args
		}
	}
	return append(args, "--ro-bind", filepath.Clean(source), dest)
}

func cliAppendTmpfsIfNotPresent(args []string, target string) []string {
	target = filepath.Clean(target)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--tmpfs" && filepath.Clean(args[i+1]) == target {
			return args
		}
	}
	return append(args, "--tmpfs", target)
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

	bwrapMissing := false
	var bwrapErr error

	bwrapPath, err := cliBwrapLookup("bwrap")
	if err != nil || os.Getenv("MAKEWAND_NO_BWRAP") == "1" {
		bwrapMissing = true
		bwrapErr = err
		if bwrapErr == nil && os.Getenv("MAKEWAND_NO_BWRAP") == "1" {
			bwrapErr = errors.New("bubblewrap disabled by MAKEWAND_NO_BWRAP=1")
		}
	} else if cliBwrapProbe != nil {
		if probeErr := cliBwrapProbe(bwrapPath); probeErr != nil {
			bwrapMissing = true
			bwrapErr = probeErr
		}
	}

	if bwrapMissing {
		// Every unsandboxed launch, whatever the task type, needs explicit
		// host-exec authorization and is audited (H1). Bubblewrap is Linux-only;
		// on Windows, process trees and resources are bounded by Job Objects
		// (processjob) but the filesystem is not isolated, so the same gate applies.
		authorized, auth := isUnsafeHostExecAuthorized(ctx)
		if isCLISandboxRequired(ctx) || !authorized {
			msg := "bubblewrap (bwrap) sandbox is not available and unsafe host execution is not acknowledged (MAKEWAND_UNSAFE_HOST_EXEC=1 alone never enables it). Execution blocked for security."
			if runtime.GOOS == "windows" {
				msg = "bubblewrap (bwrap) sandbox is not available on windows and unsafe host execution is not acknowledged (MAKEWAND_UNSAFE_HOST_EXEC=1 alone never enables it). Execution blocked for security."
			}
			if bwrapErr != nil {
				msg = fmt.Sprintf("%s: %v", msg, bwrapErr)
			}
			return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, msg, bwrapErr)
		}

		// Authorized host execution: sanitize the environment so foreign
		// credentials and host secrets are not leaked, audit, and record.
		cmd.Env = cliWithProviderWorkerEnv(sanitizeCLIEnv(provider, cmd.Env))
		cliAuditHostExec(auth, provider, cmd)
		recordCLIExec(ctx, CLIExecRecord{
			Sandboxed:  false,
			UnsafeHost: true,
			Provider:   provider,
			Workspace:  cmd.Dir,
		})
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
	readOnlyProvider := isReview || RemoteOriginFromContext(ctx)

	childEnv := cliWithProviderWorkerEnv(sanitizeCLIEnv(provider, cmd.Env))
	home := cliEnvValue(childEnv, "HOME")
	if home != "" {
		home = filepath.Clean(home)
	}

	// Broad roots, HOME, and HOME's ancestors are refused for every task: even
	// a read-only bind of such a root would re-expose the host roots and
	// credential directories the masks below hide.
	if isForbiddenWritableWorkspace(workspace, home) {
		return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0,
			fmt.Sprintf("refusing to use %s as writable sandbox workspace: protected root or restricted path (system root, HOME, or an ancestor of HOME)", workspace), nil)
	}

	// Scan the workspace before building the plan: .git metadata and unix
	// sockets inside a writable bind must be protected, and a truncated scan
	// cannot prove that, so it fails closed.
	scan := cliScanWorkspace(workspace)
	if scan.truncated && !isReview {
		return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0,
			fmt.Sprintf("workspace %s could not be fully scanned (more than %d entries, or an unreadable directory owned by you); .git metadata and unix sockets cannot be verified, refusing writable sandbox (raise MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT or fix the directory permissions)", workspace, cliWorkspaceScanLimitValue()), nil)
	}
	protectArgs, err := cliWorkspaceProtectionArgs(workspace, scan, !isReview)
	if err != nil {
		return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
	}

	var seccompPipe *os.File
	var seccompArgs []string
	seccompRequired, seccompOptedOut := cliSeccompPolicy()
	seccompUnavailable := func(reason string, cause error) error {
		if seccompRequired {
			return newProviderError(provider, "sandbox", ErrorKindConfig, false, 0,
				reason+" (set MAKEWAND_ALLOW_NO_SECCOMP=1 to run without the syscall filter)", cause)
		}
		if seccompOptedOut {
			fmt.Fprintf(os.Stderr, "makewand: WARNING: running %s CLI sandbox without seccomp filter (MAKEWAND_ALLOW_NO_SECCOMP=1): %s\n", provider, reason)
		}
		return nil
	}
	if bpf := generateSeccompBPFFilter(); len(bpf) > 0 {
		rPipe, wPipe, pipeErr := os.Pipe()
		if pipeErr == nil {
			_, _ = wPipe.Write(bpf)
			_ = wPipe.Close()
			seccompPipe = rPipe
			targetFD := 3 + len(cmd.ExtraFiles)
			seccompArgs = []string{"--seccomp", strconv.Itoa(targetFD)}
		} else if err := seccompUnavailable("failed to create seccomp pipe: "+pipeErr.Error(), pipeErr); err != nil {
			return nil, err
		}
	} else if err := seccompUnavailable("seccomp bpf unsupported on this architecture", nil); err != nil {
		return nil, err
	}

	bwrapArgs := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-user",
		// No --disable-userns (bubblewrap >= 0.8; Ubuntu 22.04 ships 0.6, RHEL 9
		// and Debian 11 0.4) and no --clearenv (>= 0.5): wrapped.Env already
		// carries only the sanitized allowlist, and provider CLIs such as codex
		// run their own bubblewrap sandbox, which needs nested user namespaces.
		// Network is NOT unshared by default: external API access is required by CLI providers.
		// MAKEWAND_SANDBOX_UNSHARE_NET explicitly isolates the network namespace to block abstract sockets & loopback.
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run",
	}

	if len(seccompArgs) > 0 {
		bwrapArgs = append(bwrapArgs, seccompArgs...)
	}

	for _, entry := range childEnv {
		if parts := strings.SplitN(entry, "=", 2); len(parts) == 2 {
			bwrapArgs = append(bwrapArgs, "--setenv", parts[0], parts[1])
		}
	}

	if os.Getenv("MAKEWAND_SANDBOX_UNSHARE_NET") == "1" || strings.ToLower(os.Getenv("MAKEWAND_SANDBOX_UNSHARE_NET")) == "true" {
		bwrapArgs = append(bwrapArgs, "--unshare-net")
	}

	var readonlyMasks []string

	// Mask host roots (/root, /media, /srv, /mnt, /home). A workspace equal to
	// one of them was refused above, so the masks always apply.
	for _, r := range []string{"/root", "/media", "/srv", "/mnt", "/home"} {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() {
			bwrapArgs = append(bwrapArgs, "--tmpfs", r)
			readonlyMasks = append(readonlyMasks, r)
		}
	}

	// Mask server state directories and config files (/var/lib/makewand, /etc/makewand, MAKEWAND_DATA_DIR, etc.)
	bwrapArgs = cliMaskServerPaths(bwrapArgs, workspace, cmd.Env)
	p := strings.TrimSuffix(strings.ToLower(provider), "-cli")
	var selectedCodexHome, realCodexHome string
	if p == "codex" {
		var err error
		selectedCodexHome, realCodexHome, err = cliConfiguredCodexHome(childEnv, workspace, home)
		if err != nil {
			return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
		}
	}

	// Every session gets a per-run private provider home (H3) holding copies
	// of allowlisted credential files, and only changed credential files are
	// written back after the sandbox exits. Host provider directories are
	// never bound writable. Writable sessions use it as a fresh 0700 HOME;
	// read-only (review and remote-origin) sessions keep an empty tmpfs HOME
	// with the private provider directories bound at their host paths, so a
	// token refresh during a review is merged back instead of being lost
	// with a refresh token the server already rotated.
	var privHome *cliPrivateHome
	planned := false
	defer func() {
		if !planned && privHome != nil {
			privHome.cleanup()
		}
	}()
	ensurePrivHome := func() error {
		if privHome != nil {
			return nil
		}
		var err error
		privHome, err = newCLIPrivateHome(workspace)
		if err != nil {
			return newProviderError(provider, "sandbox", ErrorKindConfig, false, 0,
				"failed to create private provider home: "+err.Error(), err)
		}
		return nil
	}

	var allowedCreds []string
	if home != "" {
		allowedCreds = cliProviderCredentials[p]
		if selectedCodexHome != "" {
			allowedCreds = nil // Explicit account selection does not expose the default account.
		}
		if home != "/" {
			if err := ensurePrivHome(); err != nil {
				return nil, err
			}
			if readOnlyProvider {
				bwrapArgs = append(bwrapArgs, "--tmpfs", home)
			} else {
				privHome.homeBound = true
				bwrapArgs = append(bwrapArgs, "--bind", privHome.home, home)
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

	if cliTestExtraBinds != nil {
		bwrapArgs = append(bwrapArgs, cliTestExtraBinds(targetBin, cmd.Env)...)
	}

	// Mount workspace (after host roots /tmpfs mounts so it overlays correctly)
	if isReview {
		bwrapArgs = append(bwrapArgs, "--ro-bind", workspace, workspace)
	} else {
		bwrapArgs = append(bwrapArgs, "--bind", workspace, workspace)
	}

	// Re-bind target binary and any symlink targets (essential if binary or its environment is under /tmp, /mnt, or /home)
	bwrapArgs = rebindToolchainUnderMaskedRoot(bwrapArgs, targetBin, workspace, home)

	// Expose active provider credentials within home and mask sensitive credentials
	if home != "" {
		for _, cred := range allowedCreds {
			target := filepath.Join(home, filepath.FromSlash(cred))
			if target == workspace || cliPathWithin(target, workspace) {
				continue
			}
			fi, err := os.Stat(target) //nolint:gosec // G703: checking existence of provider credentials in host home
			if err != nil {
				// A login made inside the sandbox in a directory the host
				// lacks is reported after the run, never written back.
				if privHome != nil && errors.Is(err, os.ErrNotExist) {
					if bwrapArgs, err = privHome.addAbsentProviderEntry(bwrapArgs, cred, target); err != nil {
						return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
					}
				}
				continue
			}
			if privHome != nil {
				bwrapArgs, err = privHome.addProviderEntry(bwrapArgs, p, home, cred, target, fi, workspace)
				if err != nil {
					return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
				}
				continue
			}
			bwrapArgs = cliAppendRoBindIfNotPresent(bwrapArgs, target)
			if fi.IsDir() {
				bwrapArgs = cliShieldProviderState(bwrapArgs, p, target, workspace, true, false)
			}
		}

		// Masks apply even though HOME itself is hidden: a sensitive entry
		// may be a symlink whose target lies outside HOME and outside the
		// masked host roots, where the read-only root bind would expose it.
		{
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
			if homeEntries, err := os.ReadDir(home); err == nil {
				for _, de := range homeEntries {
					name := de.Name()
					for _, pfx := range []string{".codex", ".claude", ".gemini", ".openai", ".anthropic"} {
						if strings.HasPrefix(name, pfx) {
							if !isAllowed(name) {
								toMask = append(toMask, name)
							}
						}
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
		// credentials, as a private copy (read-only sessions too). Use its
		// canonical path inside the sandbox: a destination symlink may point
		// into a masked root and cannot serve as a mount point.
		if err := ensurePrivHome(); err != nil {
			return nil, err
		}
		privCodex, err := privHome.addCodexHome(realCodexHome)
		if err != nil {
			return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0, err.Error(), err)
		}
		bwrapArgs = append(bwrapArgs, "--bind", privCodex, realCodexHome,
			"--setenv", "CODEX_HOME", realCodexHome)
		bwrapArgs = cliShieldProviderState(bwrapArgs, "codex", realCodexHome, workspace, false, true)
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

	// Protect workspace .git metadata (read-only) and mask unix sockets last,
	// so no later mount can re-expose them (H4/H5).
	bwrapArgs = append(bwrapArgs, protectArgs...)

	for _, mask := range readonlyMasks {
		bwrapArgs = append(bwrapArgs, "--remount-ro", mask)
	}

	var statusRead, statusWrite *os.File
	if privHome != nil {
		// bwrap reports container status on this descriptor; it reaches EOF
		// only once every sandbox process has exited, which triggers the
		// credential write-back.
		statusRead, statusWrite, err = os.Pipe()
		if err != nil {
			return nil, newProviderError(provider, "sandbox", ErrorKindConfig, false, 0,
				"failed to create sandbox status pipe: "+err.Error(), err)
		}
		statusFD := 3 + len(cmd.ExtraFiles)
		if seccompPipe != nil {
			statusFD++
		}
		bwrapArgs = append(bwrapArgs, "--json-status-fd", strconv.Itoa(statusFD))
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
	wrapped.ExtraFiles = append([]*os.File(nil), cmd.ExtraFiles...)
	if seccompPipe != nil {
		wrapped.ExtraFiles = append(wrapped.ExtraFiles, seccompPipe)
		runtime.SetFinalizer(seccompPipe, func(f *os.File) { _ = f.Close() })
	}
	if privHome != nil {
		wrapped.ExtraFiles = append(wrapped.ExtraFiles, statusWrite)
		privHome.watch(wrapped, statusRead, statusWrite)
	}
	planned = true
	recordCLIExec(ctx, CLIExecRecord{
		Sandboxed:  true,
		UnsafeHost: false,
		Provider:   provider,
		Workspace:  workspace,
	})

	return wrapped, nil
}

// cliChildMakewandEnvKeys is the explicit set of MAKEWAND_* variables a
// provider CLI child may see. Child CLIs do not read makewand's control or
// server variables; only non-secret run identifiers are forwarded so nested
// makewand invocations keep their task/workspace attribution.
var cliChildMakewandEnvKeys = map[string]bool{
	"MAKEWAND_TASK_ID":      true,
	"MAKEWAND_WORKSPACE_ID": true,
	cliProviderWorkerEnvKey: true,
}

// cliProviderWorkerEnvKey marks every provider CLI child launched by the
// router. The makewand Python CLI refuses model-dispatching subcommands when it
// is "1", preventing makewand -> provider CLI -> makewand recursion.
const cliProviderWorkerEnvKey = "MAKEWAND_PROVIDER_WORKER"

// cliWithProviderWorkerEnv sets MAKEWAND_PROVIDER_WORKER=1 in a sanitized
// child environment, replacing any inherited value.
func cliWithProviderWorkerEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, cliProviderWorkerEnvKey+"=") {
			out = append(out, entry)
		}
	}
	return append(out, cliProviderWorkerEnvKey+"=1")
}

// cliCodeInjectionEnvKeys are runtime/loader variables that inject code into
// the child process and are dropped regardless of allowlisted prefixes.
var cliCodeInjectionEnvKeys = map[string]bool{
	"LD_PRELOAD":                true,
	"LD_LIBRARY_PATH":           true,
	"LD_AUDIT":                  true,
	"NODE_OPTIONS":              true,
	"NODE_REPL_EXTERNAL_MODULE": true,
	"PYTHONSTARTUP":             true,
	"PYTHONINSPECT":             true,
}

// cliSeccompPolicy reports whether the seccomp filter is mandatory and whether
// the operator explicitly opted out. The filter is required by default on
// linux/amd64 and linux/arm64, where it is always generated; elsewhere it is
// required only with MAKEWAND_REQUIRE_SECCOMP=1. MAKEWAND_ALLOW_NO_SECCOMP=1
// lets a launch proceed (with a stderr warning) when the filter cannot be
// installed; MAKEWAND_REQUIRE_SECCOMP=1 overrides that opt-out.
func cliSeccompPolicy() (required, optedOut bool) {
	defaultRequired := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	if os.Getenv("MAKEWAND_REQUIRE_SECCOMP") == "1" {
		return true, false
	}
	if defaultRequired && os.Getenv("MAKEWAND_ALLOW_NO_SECCOMP") == "1" {
		return false, true
	}
	return defaultRequired, false
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
		if strings.HasPrefix(upper, "DBUS_") || upper == "DISPLAY" || upper == "XAUTHORITY" || upper == "WAYLAND_DISPLAY" {
			return true
		}
		if strings.HasPrefix(upper, "SSH_") || strings.HasPrefix(upper, "GPG_") {
			return true
		}
		if strings.HasPrefix(upper, "MAKEWAND_SERVER_") ||
			upper == "MAKEWAND_AUTH_CONFIG" ||
			upper == "MAKEWAND_DATA_DIR" ||
			upper == "MAKEWAND_CONFIG_DIR" ||
			upper == "MAKEWAND_ADMIN_SESSION_SECRET" ||
			upper == "MAKEWAND_STATE_DB" {
			return true
		}
		for _, prefix := range []string{
			"AWS_", "AZURE_", "GCP_", "SENDGRID_", "STRIPE_", "SLACK_", "TWILIO_",
			"MAILGUN_", "GITHUB_", "GITLAB_", "GITEA_", "BITBUCKET_", "VAULT_",
			"POSTGRES_", "MYSQL_", "MONGO_", "REDIS_", "SENTRY_", "DATADOG_",
		} {
			if strings.HasPrefix(upper, prefix) {
				return true
			}
		}
		for _, db := range []string{"DATABASE_URL", "DB_PASSWORD", "DATABASE_PASSWORD", "PGPASSWORD", "MYSQL_PWD", "MONGO_URI", "REDIS_URL", "REDIS_PASSWORD"} {
			if upper == db {
				return true
			}
		}
		for _, s := range []string{"GH_TOKEN", "GL_TOKEN", "DD_API_KEY", "NPM_TOKEN"} {
			if upper == s {
				return true
			}
		}
		for _, pattern := range []string{
			"_SECRET", "_PASSWORD", "_PASSWD", "_TOKEN", "_PRIVATE_KEY",
			"_API_KEY", "_APIKEY", "_KEY", "_AUTH", "_CREDENTIAL", "_SIGNING",
		} {
			if strings.Contains(upper, pattern) {
				return true
			}
		}
		return false
	}

	isAllowedSystemKey := func(key string) bool {
		upper := strings.ToUpper(key)
		switch {
		case upper == "PATH" || upper == "HOME" || upper == "USER" || upper == "LOGNAME" || upper == "SHELL" ||
			upper == "TERM" || upper == "TMPDIR" || upper == "TMP" || upper == "TEMP" || upper == "TZ" ||
			upper == "PWD" || upper == "SHLVL" || upper == "_" || upper == "LANG" || upper == "CI" || upper == "HOSTNAME":
			return true
		case strings.HasPrefix(upper, "LC_"):
			return true
		case upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY":
			return true
		case key == "http_proxy" || key == "https_proxy" || key == "all_proxy" || key == "no_proxy":
			return true
		case strings.HasPrefix(upper, "GIT_") || upper == "EDITOR" || upper == "VISUAL":
			return true
		case cliCodeInjectionEnvKeys[upper]:
			// Variables that make the child's runtime load attacker-chosen code
			// (NODE_OPTIONS=--require, PYTHONSTARTUP, ...) never pass, even
			// under an allowed prefix.
			return false
		case strings.HasPrefix(upper, "NODE_") || strings.HasPrefix(upper, "PYTHON") ||
			strings.HasPrefix(upper, "GO") || strings.HasPrefix(upper, "CARGO_") ||
			strings.HasPrefix(upper, "RUST"):
			return true
		case cliChildMakewandEnvKeys[upper]:
			return true
		case cliTestEnvAllowed != nil && cliTestEnvAllowed(key):
			return true
		default:
			return false
		}
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

		if !isAllowedSystemKey(key) {
			continue
		}

		sanitized = append(sanitized, entry)
	}

	return sanitized
}
