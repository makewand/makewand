package engine

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeExec creates an executable file (and its parent directories).
func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: toolchain fixtures must be executable to be found on PATH.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stubGoEnv(t *testing.T, env hostGoEnv, err error) {
	t.Helper()
	old := sandboxGoEnvProbe
	t.Cleanup(func() { sandboxGoEnvProbe = old })
	sandboxGoEnvProbe = func(string) (hostGoEnv, error) { return env, err }
}

// roBinds returns the sources of every --ro-bind in args.
func roBinds(args []string) []string {
	var out []string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func envValue(layout sandboxHomeLayout, key string) string {
	for i := 0; i+1 < len(layout.env); i += 2 {
		if layout.env[i] == key {
			return layout.env[i+1]
		}
	}
	return ""
}

// fakeToolchainHome builds a HOME containing credentials and toolchains laid
// out the way nvm, a Go SDK, rustup, pipx and a virtualenv install them.
func fakeToolchainHome(t *testing.T) (home, pathEnv string) {
	t.Helper()
	home = t.TempDir()
	// Credentials and history that must never be re-bound.
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), `{"accessToken":"x"}`)
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), "{}")
	writeFile(t, filepath.Join(home, ".git-credentials"), "https://u:t@example.com")
	writeFile(t, filepath.Join(home, ".cargo", "credentials.toml"), "token = 'x'")
	writeFile(t, filepath.Join(home, ".local", "share", "makewand", "state"), "x")
	writeFile(t, filepath.Join(home, ".bash_history"), "secret")
	// nvm
	writeExec(t, filepath.Join(home, ".nvm", "versions", "node", "v26", "bin", "node"))
	npmCLI := filepath.Join(home, ".nvm", "versions", "node", "v26", "lib", "node_modules", "npm", "bin", "npm-cli.js")
	writeExec(t, npmCLI)
	if err := os.Symlink("../lib/node_modules/npm/bin/npm-cli.js", filepath.Join(home, ".nvm", "versions", "node", "v26", "bin", "npm")); err != nil {
		t.Fatal(err)
	}
	// Go SDK in ~/sdk/go1.27 and a module cache.
	writeExec(t, filepath.Join(home, "sdk", "go1.27", "bin", "go"))
	if err := os.MkdirAll(filepath.Join(home, "gopath", "pkg", "mod", "cache", "download"), 0o755); err != nil {
		t.Fatal(err)
	}
	// rustup: proxies in ~/.cargo/bin, toolchains in ~/.rustup.
	writeExec(t, filepath.Join(home, ".cargo", "bin", "cargo"))
	if err := os.MkdirAll(filepath.Join(home, ".rustup", "toolchains"), 0o755); err != nil {
		t.Fatal(err)
	}
	// pipx: ~/.local/bin/pytest -> ~/.local/share/pipx/venvs/pytest/bin/pytest
	writeExec(t, filepath.Join(home, ".local", "share", "pipx", "venvs", "pytest", "bin", "pytest"))
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".local", "share", "pipx", "venvs", "pytest", "bin", "pytest"), filepath.Join(home, ".local", "bin", "pytest")); err != nil {
		t.Fatal(err)
	}
	// virtualenv python
	writeExec(t, filepath.Join(home, "venv", "bin", "python3"))
	writeFile(t, filepath.Join(home, "venv", "pyvenv.cfg"), "home = /usr/bin\n")

	pathEnv = strings.Join([]string{
		filepath.Join(home, "venv", "bin"),
		filepath.Join(home, ".nvm", "versions", "node", "v26", "bin"),
		filepath.Join(home, "sdk", "go1.27", "bin"),
		filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, ".local", "bin"),
		"/usr/bin", "/bin",
	}, string(os.PathListSeparator))
	return home, pathEnv
}

func TestSandboxHomeLayout_RebindsToolchainsReadOnlyWithoutCredentials(t *testing.T) {
	home, pathEnv := fakeToolchainHome(t)
	stubGoEnv(t, hostGoEnv{
		GOROOT:     filepath.Join(home, "sdk", "go1.27"),
		GOMODCACHE: filepath.Join(home, "gopath", "pkg", "mod"),
		GOVERSION:  "go1.27.1",
		GOPROXY:    "https://corp.example/proxy,https://goproxy.io,direct",
	}, nil)
	workspace := filepath.Join(home, "dev", "proj")

	layout := buildSandboxHomeLayout(home, workspace, pathEnv, func(string) string { return "" })
	if len(layout.beforeWorkspace) < 2 || layout.beforeWorkspace[0] != "--tmpfs" || layout.beforeWorkspace[1] != home {
		t.Fatalf("layout must start by hiding HOME: %v", layout.beforeWorkspace)
	}
	binds := roBinds(layout.beforeWorkspace)
	for _, want := range []string{
		filepath.Join(home, ".nvm", "versions", "node", "v26"),
		filepath.Join(home, "sdk", "go1.27"),
		filepath.Join(home, "gopath", "pkg", "mod", "cache", "download"),
		filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, ".rustup"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".local", "share", "pipx", "venvs", "pytest"),
		filepath.Join(home, "venv"),
	} {
		found := false
		for _, b := range binds {
			if b == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing read-only toolchain bind %s; binds=%v", want, binds)
		}
	}
	for _, b := range binds {
		if b == home || sandboxPathTouchesSensitive(home, b) {
			t.Errorf("bind %s would expose HOME or a credential store", b)
		}
		if _, err := os.Stat(b); err != nil {
			t.Errorf("bind source %s does not exist (bwrap would fail): %v", b, err)
		}
	}
	if got := envValue(layout, "GOTOOLCHAIN"); got != "local" {
		t.Errorf("GOTOOLCHAIN = %q, want local", got)
	}
	proxy := envValue(layout, "GOPROXY")
	if !strings.HasPrefix(proxy, "file://"+filepath.ToSlash(filepath.Join(home, "gopath", "pkg", "mod", "cache", "download"))+",") {
		t.Errorf("GOPROXY = %q, want the host module cache as first (offline) proxy", proxy)
	}
	if strings.Contains(proxy, "secret") {
		t.Errorf("GOPROXY leaked credentials into the sandbox: %q", proxy)
	}
	if got := envValue(layout, "RUSTUP_HOME"); got != filepath.Join(home, ".rustup") {
		t.Errorf("RUSTUP_HOME = %q, want %s", got, filepath.Join(home, ".rustup"))
	}
	if layout.goVersion != "go1.27.1" {
		t.Errorf("goVersion = %q, want go1.27.1", layout.goVersion)
	}
}

func TestSandboxHomeLayout_WorkspaceUnderHomeIsBoundAfterHomeIsHidden(t *testing.T) {
	fakeWorkingBwrap(t)
	home := t.TempDir()
	verifyUserHome = func() (string, error) { return home, nil }
	workspace := filepath.Join(home, "dev", "proj")
	// .netrc is a file, .aws is missing: both broke the old per-entry masks.
	writeFile(t, filepath.Join(home, ".netrc"), "machine x")

	_, args := wrapVerificationCommand("/usr/bin/bwrap", workspace, "go", []string{"test", "./..."}, false, nil)
	tmpfsIdx := argPairIndex(args, "--tmpfs", home)
	bindIdx := argPairIndex(args, "--bind", workspace)
	if tmpfsIdx < 0 || bindIdx < 0 || tmpfsIdx > bindIdx {
		t.Fatalf("HOME tmpfs (idx %d) must precede the workspace bind (idx %d): %v", tmpfsIdx, bindIdx, args)
	}
	for _, entry := range []string{".netrc", ".aws", ".ssh"} {
		target := filepath.Join(home, entry)
		if containsArgPair(args, "--tmpfs", target) {
			t.Fatalf("per-entry tmpfs on %s would need a mount point on the read-only root: %v", target, args)
		}
	}
}

func TestSandboxHomeLayout_WorkspaceIsHomeMasksEntriesByType(t *testing.T) {
	stubGoEnv(t, hostGoEnv{}, errors.New("no go"))
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".netrc"), "machine x")
	writeFile(t, filepath.Join(home, ".ssh", "id_ed25519"), "key")
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), "{}")
	// .aws deliberately missing

	layout := buildSandboxHomeLayout(home, home, "/usr/bin:/bin", func(string) string { return "" })
	if len(layout.beforeWorkspace) != 0 {
		t.Fatalf("HOME == workspace must not tmpfs HOME before the bind: %v", layout.beforeWorkspace)
	}
	after := layout.afterWorkspace
	if !containsArgPair(after, os.DevNull, filepath.Join(home, ".netrc")) {
		t.Errorf("file entry .netrc must be shadowed by /dev/null: %v", after)
	}
	if !containsArgPair(after, "--tmpfs", filepath.Join(home, ".ssh")) {
		t.Errorf("directory entry .ssh must get a tmpfs: %v", after)
	}
	if !containsArgPair(after, "--tmpfs", filepath.Join(home, ".claude")) {
		t.Errorf("AI CLI credentials .claude must be masked: %v", after)
	}
	for _, arg := range after {
		if strings.Contains(arg, ".aws") {
			t.Errorf("missing entry .aws must not become a mount point: %v", after)
		}
	}
}

func TestSandboxHomeLayout_MissingHomeProducesNoMounts(t *testing.T) {
	stubGoEnv(t, hostGoEnv{}, errors.New("no go"))
	missing := filepath.Join(t.TempDir(), "gone")
	layout := buildSandboxHomeLayout(missing, "/tmp/ws", "/usr/bin:/bin", func(string) string { return "" })
	if len(layout.beforeWorkspace)+len(layout.afterWorkspace) != 0 {
		t.Fatalf("a missing HOME must not produce mounts (bwrap cannot mkdir on the read-only root): %+v", layout)
	}
}

func TestSanitizeGoProxyList(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"https://proxy.golang.org,direct": "https://proxy.golang.org,direct",
		"https://u:p@corp/proxy,https://a,direct": "https://a,direct",
		"https://a|https://u:p@b|direct":          "https://a|direct",
		"off":                                     "off",
	} {
		if got := sanitizeGoProxyList(in); got != want {
			t.Errorf("sanitizeGoProxyList(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckSandboxGoToolchain(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "go.mod"), "module x\n\ngo 1.26 // comment\n")
	if err := checkSandboxGoToolchain(ws, "go1.27.1"); err != nil {
		t.Fatalf("newer host toolchain rejected: %v", err)
	}
	if err := checkSandboxGoToolchain(ws, "go1.26"); err != nil {
		t.Fatalf("equal host toolchain rejected: %v", err)
	}
	err := checkSandboxGoToolchain(ws, "go1.22.2")
	if err == nil || !strings.Contains(err.Error(), "go1.26") || !strings.Contains(err.Error(), "go1.22.2") {
		t.Fatalf("older host toolchain: err = %v, want a clear requirement error", err)
	}
	if err := checkSandboxGoToolchain(t.TempDir(), "go1.22.2"); err != nil {
		t.Fatalf("workspace without go.mod: %v", err)
	}
}

func TestIsBwrapSetupFailure(t *testing.T) {
	if !isBwrapSetupFailure(&ExecResult{ExitCode: 1, Stderr: "bwrap: Can't mkdir /home/alice/.aws: Read-only file system\n"}) {
		t.Fatal("bwrap mount failure not recognized")
	}
	if isBwrapSetupFailure(&ExecResult{ExitCode: 1, Stderr: "FAIL: TestX\n"}) {
		t.Fatal("ordinary test failure misclassified as sandbox failure")
	}
	if isBwrapSetupFailure(&ExecResult{ExitCode: 1, Stdout: "ran", Stderr: "bwrap: x"}) {
		t.Fatal("a command that produced output did start")
	}
}

func TestSandboxWorkspaceSocketMasks_IncludesIgnoredDirectories(t *testing.T) {
	ws := t.TempDir()
	gitDir := filepath.Join(ws, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockInGit := filepath.Join(gitDir, "daemon.sock")
	l1, err := net.Listen("unix", sockInGit)
	if err != nil {
		t.Skipf("cannot create unix socket: %v", err)
	}
	defer l1.Close()

	nodeDir := filepath.Join(ws, "node_modules", "daemon")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockInNode := filepath.Join(nodeDir, "ipc.sock")
	l2, err := net.Listen("unix", sockInNode)
	if err != nil {
		t.Skipf("cannot create unix socket: %v", err)
	}
	defer l2.Close()

	masks := sandboxWorkspaceSocketMasks(ws)
	foundGit := false
	foundNode := false
	for i, arg := range masks {
		if arg == sockInGit && i > 0 && masks[i-1] == os.DevNull {
			foundGit = true
		}
		if arg == sockInNode && i > 0 && masks[i-1] == os.DevNull {
			foundNode = true
		}
	}
	if !foundGit {
		t.Errorf("socket in .git (%s) was not masked in: %v", sockInGit, masks)
	}
	if !foundNode {
		t.Errorf("socket in node_modules (%s) was not masked in: %v", sockInNode, masks)
	}
}

func TestSanitizeGitExecEnv(t *testing.T) {
	inputEnv := []string{
		"PATH=/usr/bin:/bin",
		"GIT_DIR=/evil/git/dir",
		"GIT_WORK_TREE=/evil/wt",
		"GIT_INDEX_FILE=/evil/index",
		"GIT_EXTERNAL_DIFF=/tmp/evil_diff",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.fsmonitor",
		"GIT_CONFIG_VALUE_0=evil.sh",
		"GIT_SSH_COMMAND=ssh -o ProxyCommand=evil",
		"USER=testuser",
	}
	cleaned := sanitizeGitExecEnv(inputEnv)
	for _, entry := range cleaned {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		k := entry[:eq]
		if isDangerousGitEnv(k) {
			t.Errorf("dangerous git env %q not stripped: %s", k, entry)
		}
	}
	if !hasEnvKey(cleaned, "GIT_OPTIONAL_LOCKS") {
		t.Errorf("GIT_OPTIONAL_LOCKS=0 was not added")
	}
	if !hasEnvKey(cleaned, "USER") {
		t.Errorf("USER was improperly stripped")
	}
}
