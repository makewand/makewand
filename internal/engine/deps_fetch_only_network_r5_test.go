//go:build linux

package engine

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// GEC-1021-1 round 5 (final verification of PR #91, round 2): the networked
// cargo fetch is pinned to a named rustup toolchain, so a workspace
// rust-toolchain file can no longer make it run a toolchain from a path.

// rustupHomeForTest points the verification environment at a rustup home of
// the test's own whose settings.toml holds settings, and returns it.
func rustupHomeForTest(t *testing.T, settings string) string {
	t.Helper()
	swapVerifyIsolationVars(t)
	home := t.TempDir()
	rustupHome := filepath.Join(home, "rustup")
	writeFile(t, filepath.Join(rustupHome, "settings.toml"), settings)
	verifyUserHome = func() (string, error) { return home, nil }
	verifyGetenv = func(key string) string {
		switch key {
		case "RUSTUP_HOME":
			return rustupHome
		case "PATH":
			return "/usr/bin:/bin"
		}
		return ""
	}
	return rustupHome
}

// rustup 1.28 reads a toolchain path from every form below (each one made a
// marker toolchain run in a no-network sandbox on this host), so each must be
// seen as a path; channels are taken as written.
func TestParseRustToolchainFile_GEC1021R5(t *testing.T) {
	for _, tc := range []struct {
		name, file, content string
		channel             string
		path                bool
	}{
		{"table path", "rust-toolchain.toml", "[toolchain]\npath = \"/ws/evil\"\n", "", true},
		{"dotted key", "rust-toolchain.toml", "toolchain.path = \"/ws/evil\"\n", "", true},
		{"quoted key", "rust-toolchain.toml", "[toolchain]\n\"path\" = \"/ws/evil\"\n", "", true},
		{"quoted header", "rust-toolchain.toml", "[\"toolchain\"]\npath = \"/ws/evil\"\n", "", true},
		{"spaced header", "rust-toolchain.toml", "[ toolchain ]\npath = '/ws/evil'\n", "", true},
		{"inline table", "rust-toolchain.toml", "toolchain = { path = \"/ws/evil\" }\n", "", true},
		{"multi-line string", "rust-toolchain.toml", "[toolchain]\npath = \"\"\"/ws/evil\"\"\"\n", "", true},
		{"relative path", "rust-toolchain.toml", "[toolchain]\npath = \"./evil\"\n", "", true},
		{"legacy absolute path", "rust-toolchain", "/ws/evil\n", "", true},
		{"legacy TOML path", "rust-toolchain", "[toolchain]\npath = \"/ws/evil\"\n", "", true},
		{"channel", "rust-toolchain.toml", "[toolchain]\nchannel = \"1.85.0\"\ncomponents = [\"rustfmt\"]\n", "1.85.0", false},
		{"inline channel", "rust-toolchain.toml", "toolchain = { channel = \"nightly-2024-01-01\" }\n", "nightly-2024-01-01", false},
		{"legacy channel", "rust-toolchain", "stable\n", "stable", false},
		{"legacy relative path is a channel", "rust-toolchain", "./evil\n", "./evil", false},
		{"components only", "rust-toolchain.toml", "[toolchain]\ncomponents = [\"clippy\"]\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRustToolchainFile(tc.file, tc.content)
			if got.path != tc.path || got.channel != tc.channel {
				t.Fatalf("parse %q = %+v, want channel %q path %v", tc.content, got, tc.channel, tc.path)
			}
		})
	}
}

// The pin is the toolchain rustup would pick, never a path: the workspace's
// directory override, the toolchain file's channel, an override above the
// workspace, the default. A path, a channel that is not one, or a file the pin
// cannot replace for lack of a default refuses the networked fetch.
func TestCargoFetchToolchain_GEC1021R5(t *testing.T) {
	const defaults = "version = \"12\"\ndefault_toolchain = \"stable-x86_64-unknown-linux-gnu\"\n"
	for _, tc := range []struct {
		name      string
		settings  string
		files     []string
		overrides func(ws string) string
		pin       string
		refused   string // a substring of the refusal
	}{
		{name: "no toolchain file uses the default", settings: defaults, pin: "stable-x86_64-unknown-linux-gnu"},
		{name: "channel file", settings: defaults, files: []string{"rust-toolchain.toml", "[toolchain]\nchannel = \"1.85.0\"\n"}, pin: "1.85.0"},
		{name: "dated channel with host", settings: defaults, files: []string{"rust-toolchain", "nightly-2024-01-01-x86_64-unknown-linux-gnu\n"}, pin: "nightly-2024-01-01-x86_64-unknown-linux-gnu"},
		{name: "legacy file wins over the TOML one", settings: defaults, files: []string{"rust-toolchain", "1.85.0\n", "rust-toolchain.toml", "[toolchain]\npath = \"/ws/evil\"\n"}, pin: "1.85.0"},
		{name: "absolute path", settings: defaults, files: []string{"rust-toolchain.toml", "[toolchain]\npath = \"/ws/evil\"\n"}, refused: "selects a rustup toolchain by path"},
		{name: "relative path", settings: defaults, files: []string{"rust-toolchain.toml", "[toolchain]\npath = \"evil\"\n"}, refused: "by path"},
		{name: "legacy path", settings: defaults, files: []string{"rust-toolchain", "/ws/evil\n"}, refused: "by path"},
		{name: "channel with a slash", settings: defaults, files: []string{"rust-toolchain.toml", "[toolchain]\nchannel = \"../evil\"\n"}, refused: "not a channel"},
		{name: "custom toolchain name", settings: defaults, files: []string{"rust-toolchain", "mytoolchain\n"}, refused: "not a channel"},
		{name: "file without a channel uses the default", settings: defaults, files: []string{"rust-toolchain.toml", "[toolchain]\ncomponents = [\"clippy\"]\n"}, pin: "stable-x86_64-unknown-linux-gnu"},
		{name: "file without a default cannot be pinned", settings: "version = \"12\"\n", files: []string{"rust-toolchain.toml", "[toolchain]\ncomponents = [\"clippy\"]\n"}, refused: "no default toolchain"},
		{name: "no file and no default needs no pin", settings: "version = \"12\"\n"},
		{name: "workspace override beats the file", settings: defaults, files: []string{"rust-toolchain.toml", "[toolchain]\npath = \"/ws/evil\"\n"},
			overrides: func(ws string) string { return "\n[overrides]\n\"" + ws + "\" = \"beta-x86_64-unknown-linux-gnu\"\n" }, pin: "beta-x86_64-unknown-linux-gnu"},
		{name: "override above the workspace beats the default", settings: defaults,
			overrides: func(ws string) string { return "\n[overrides]\n\"" + filepath.Dir(ws) + "\" = \"nightly\"\n" }, pin: "nightly"},
		{name: "a path override is not used", settings: defaults,
			overrides: func(ws string) string { return "\n[overrides]\n\"" + ws + "\" = \"/opt/toolchain\"\n" }, pin: "stable-x86_64-unknown-linux-gnu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := newNeedProject(t, append([]string{"Cargo.toml", "[package]\nname = \"x\"\n"}, tc.files...)...)
			settings := tc.settings
			if tc.overrides != nil {
				settings += tc.overrides(project.Path)
			}
			rustupHomeForTest(t, settings)
			pin, refusal := project.cargoFetchToolchain(project.Path)
			if pin != tc.pin {
				t.Fatalf("pin = %q (refusal %q), want %q", pin, refusal, tc.pin)
			}
			if tc.refused == "" && refusal != "" || tc.refused != "" && !strings.Contains(refusal, tc.refused) {
				t.Fatalf("refusal = %q, want %q", refusal, tc.refused)
			}
		})
	}
	t.Run("no rustup home: nothing to pin or refuse", func(t *testing.T) {
		project := newNeedProject(t, "Cargo.toml", "[package]\nname = \"x\"\n", "rust-toolchain.toml", "[toolchain]\npath = \"/ws/evil\"\n")
		rustupHomeForTest(t, defaults)
		verifyGetenv = func(string) string { return "" } // RUSTUP_HOME unset and no ~/.rustup
		if pin, refusal := project.cargoFetchToolchain(project.Path); pin != "" || refusal != "" {
			t.Fatalf("pin = %q, refusal = %q, want neither without rustup", pin, refusal)
		}
	})
}

// A relative RUSTUP_HOME would resolve against the workspace inside the
// sandbox, letting the project supply rustup's settings and toolchains, so the
// sandbox never hands it on: it falls back to ~/.rustup.
func TestSandboxIgnoresRelativeRustupHome_GEC1021R5(t *testing.T) {
	home, pathEnv := fakeToolchainHome(t)
	stubGoEnv(t, hostGoEnv{}, os.ErrNotExist)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "rel-rustup"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd) // where makewand would find the relative directory
	layout := buildSandboxHomeLayout(home, filepath.Join(home, "dev", "proj"), pathEnv, func(key string) string {
		if key == "RUSTUP_HOME" {
			return "rel-rustup"
		}
		return ""
	}, false)
	if got, want := envValue(layout, "RUSTUP_HOME"), filepath.Join(home, ".rustup"); got != want {
		t.Fatalf("sandbox RUSTUP_HOME = %q, want %q", got, want)
	}
}

func TestHostRustupHomeIgnoresRelative_GEC1021R5(t *testing.T) {
	env := func(v string) func(string) string {
		return func(key string) string {
			if key == "RUSTUP_HOME" {
				return v
			}
			return ""
		}
	}
	if got := hostRustupHome("/home/alice", env("rel/rustup")); got != "/home/alice/.rustup" {
		t.Fatalf("relative RUSTUP_HOME: %q, want the HOME default", got)
	}
	if got := hostRustupHome("/home/alice", env("/opt/rustup/")); got != "/opt/rustup" {
		t.Fatalf("absolute RUSTUP_HOME: %q", got)
	}
	if got := hostRustupHome("", env("")); got != "" {
		t.Fatalf("no HOME: %q, want none", got)
	}
}

// With outbound network available, a toolchain file naming a path refuses the
// networked cargo fetch before anything runs (an environment verdict), and a
// channel file pins the fetch's RUSTUP_TOOLCHAIN to that channel.
func TestCargoFetchToolchainEnforced_GEC1021R5(t *testing.T) {
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	run := func(t *testing.T, files ...string) (fakeUserNetEnv, error) {
		t.Helper()
		env := installFakeUserNet(t, fakeUserNetOptions{})
		rustupHome := filepath.Join(t.TempDir(), "rustup")
		writeFile(t, filepath.Join(rustupHome, "settings.toml"), "version = \"12\"\ndefault_toolchain = \"stable-x86_64-unknown-linux-gnu\"\n")
		getenv := verifyGetenv
		verifyGetenv = func(key string) string {
			if key == "RUSTUP_HOME" {
				return rustupHome
			}
			return getenv(key)
		}
		project, err := NewProject("cargo-pin", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		writeProjectFiles(t, project.Path, append([]string{"Cargo.toml", "[package]\nname = \"x\"\nversion = \"0.1.0\"\n\n[dependencies]\nserde = \"1\"\n"}, files...)...)
		_, err = project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"build"}})
		return env, err
	}
	t.Run("path toolchain is refused and cargo never runs", func(t *testing.T) {
		env, err := run(t, "rust-toolchain.toml", "[toolchain]\npath = \"/ws/evil\"\n")
		if !errorIsDepNetworkUnavailable(err) || !strings.Contains(err.Error(), "by path") {
			t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable naming the path toolchain", err)
		}
		if data := readLog(t, env.log); strings.Contains(data, "ran") {
			t.Fatalf("no sandbox may run when the fetch is refused; bwrap log:\n%s", data)
		}
	})
	for _, tc := range []struct {
		name  string
		files []string
		pin   string
	}{
		{"channel file pins the fetch", []string{"rust-toolchain.toml", "[toolchain]\nchannel = \"1.85.0\"\n"}, "1.85.0"},
		{"no file pins the default", nil, "stable-x86_64-unknown-linux-gnu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := run(t, tc.files...)
			if errorIsDepNetworkUnavailable(err) {
				t.Fatalf("err = %v, want the fetch to run", err)
			}
			data := readLog(t, env.log)
			if !regexp.MustCompile(`setenv RUSTUP_TOOLCHAIN=` + regexp.QuoteMeta(tc.pin) + `\n`).MatchString(data) {
				t.Fatalf("the networked fetch must pin RUSTUP_TOOLCHAIN=%s; bwrap log:\n%s", tc.pin, data)
			}
		})
	}
}

// Live, with the real rustup, bubblewrap and slirp4netns of this host: a
// rust-toolchain.toml naming a path toolchain in the workspace never runs its
// marker cargo or rustc during the networked fetch (which is refused), and a
// channel file pins the fetch to that channel. Wrappers on the sandbox PATH in
// front of the rustup proxies record the RUSTUP_TOOLCHAIN makewand set (cargo)
// and the toolchain rustup resolved (rustc, which cargo runs as RUSTC).
func TestCargoFetchWorkspaceToolchainLive_GEC1021R5(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("no cargo on PATH")
	}
	proxy, err := filepath.EvalSymlinks(cargo)
	if err != nil || filepath.Base(proxy) != "rustup" {
		t.Skipf("cargo %s is not a rustup proxy", cargo)
	}
	rustupHome := os.Getenv("RUSTUP_HOME")
	if rustupHome == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no HOME")
		}
		rustupHome = filepath.Join(userHome, ".rustup")
	}
	settings := readRustupSettings(rustupHome)
	if settings.defaultToolchain == "" {
		t.Skip("rustup has no default toolchain")
	}
	requireUserModeNetwork(t)
	unsetProxyEnv(t)
	t.Setenv("RUSTUP_HOME", rustupHome)
	t.Setenv("RUSTUP_TOOLCHAIN", "")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	// An installed toolchain other than the default, named by its channel.
	channel := ""
	entries, _ := os.ReadDir(filepath.Join(rustupHome, "toolchains"))
	for _, entry := range entries {
		if m := regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)-`).FindStringSubmatch(entry.Name()); m != nil && entry.Name() != settings.defaultToolchain {
			channel = m[1]
			break
		}
	}

	open := func(t *testing.T, toolchainFile string) (*Project, string) {
		t.Helper()
		project, err := NewProject("cargo-live", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		// The wrappers live outside the workspace (a fetch never runs a
		// workspace executable) and record to the shared dependency cache, the
		// only place a networked fetch writes that outlives it.
		tools := filepath.Join(os.Getenv("HOME"), "tools-"+rand.Text()) // the test's HOME, re-bound read-only
		toolchainLog := fetchLogPath(t, "toolchain")
		for _, name := range []string{"cargo", "rustc"} {
			writeExecScript(t, filepath.Join(tools, name), `echo "`+name+` RUSTUP_TOOLCHAIN=$RUSTUP_TOOLCHAIN" >> "`+toolchainLog+`"
exec `+filepath.Join(filepath.Dir(cargo), name)+` "$@"
`)
		}
		evil := filepath.Join(project.Path, "evil", "bin")
		for _, name := range []string{"cargo", "rustc"} {
			writeExecScript(t, filepath.Join(evil, name), `echo "workspace `+name+` ran" >> "`+filepath.Join(project.Path, "PWNED")+`"
exit 1
`)
		}
		files := []string{"Cargo.toml", "[package]\nname = \"x\"\nversion = \"0.1.0\"\nedition = \"2021\"\n", "src/main.rs", "fn main() {}\n"}
		if toolchainFile != "" {
			files = append(files, "rust-toolchain.toml", strings.ReplaceAll(toolchainFile, "<ws>", project.Path))
		}
		writeProjectFiles(t, project.Path, files...)
		t.Setenv("PATH", tools+":"+filepath.Dir(cargo)+":/usr/bin:/bin")
		return project, toolchainLog
	}
	fetch := ExecPlan{Kind: "deps", Detector: "Cargo.toml", Command: "cargo", Args: []string{"fetch"}}

	for _, file := range []string{"[toolchain]\npath = \"<ws>/evil\"\n", "toolchain = { path = \"<ws>/evil\" }\n"} {
		t.Run("path toolchain never runs: "+strings.SplitN(file, "=", 2)[0], func(t *testing.T) {
			project, log := open(t, file)
			_, err := project.RunVerificationPlan(context.Background(), fetch)
			if !errorIsDepNetworkUnavailable(err) {
				t.Fatalf("err = %v, want the networked fetch refused for the path toolchain", err)
			}
			if data, _ := os.ReadFile(filepath.Join(project.Path, "PWNED")); len(data) > 0 {
				t.Fatalf("the workspace toolchain ran during the fetch: %s", data)
			}
			if data := readLog(t, log); data != "" {
				t.Fatalf("cargo must not run at all when the fetch is refused: %s", data)
			}
		})
	}
	t.Run("no toolchain file pins the default", func(t *testing.T) {
		project, log := open(t, "")
		if _, err := project.RunVerificationPlan(context.Background(), fetch); err != nil {
			t.Fatalf("RunVerificationPlan: %v", err)
		}
		data := readLog(t, log)
		if !strings.Contains(data, "cargo RUSTUP_TOOLCHAIN="+settings.defaultToolchain+"\n") {
			t.Fatalf("the fetch must be pinned to the default %s; log:\n%s", settings.defaultToolchain, data)
		}
	})
	t.Run("a channel file pins the fetch to that channel", func(t *testing.T) {
		if channel == "" {
			t.Skip("no second installed rustup toolchain")
		}
		project, log := open(t, "[toolchain]\nchannel = \""+channel+"\"\n")
		if _, err := project.RunVerificationPlan(context.Background(), fetch); err != nil {
			t.Fatalf("RunVerificationPlan: %v", err)
		}
		data := readLog(t, log)
		lines := strings.Split(strings.TrimSpace(data), "\n")
		if !slices.Contains(lines, "cargo RUSTUP_TOOLCHAIN="+channel) {
			t.Fatalf("makewand must pin RUSTUP_TOOLCHAIN=%s for the fetch; log:\n%s", channel, data)
		}
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "rustc RUSTUP_TOOLCHAIN="+channel+"-") }) {
			t.Fatalf("cargo must run the %s rustc; log:\n%s", channel, data)
		}
	})
}

// pipExtrasProject is a pyproject.toml with build requirements, dependencies
// and optional-dependency groups, two of which reference each other through
// the project's own name (a cycle).
const pipExtrasProject = "[build-system]\nrequires = [\"builddep\"]\nbuild-backend = \"backend\"\nbackend-path = [\"_backend\"]\n\n" +
	"[project]\nname = \"Proj\"\nversion = \"0.1\"\ndependencies = [\"rundep\"]\n\n" +
	"[project.optional-dependencies]\nDev = [\"devdep\", \"proj[test]\"]\n" +
	"test = [\"testdep ; python_version >= '3'\", \"proj[dev]\", \"proj\"]\n" +
	"docs = [\"docdep\"]\n"

// A local project with extras - `.[dev]`, `-e .[dev]` or `./sub[docs]`, on the
// command line or in a requirements file - hands the networked fetch its build
// requirements, its dependencies and the optional-dependency groups the extras
// select, following self-references (once each, so a cycle ends) and never
// passing the project's own name to the index.
func TestPipLocalProjectExtrasReachFetch_GEC1021R5(t *testing.T) {
	selected := []string{"builddep", "rundep", "devdep", "testdep ; python_version >= '3'"}
	for _, tc := range []struct {
		name   string
		files  []string
		args   []string
		want   []string
		forbid []string
	}{
		{"-e .[dev] on the command line", []string{"pyproject.toml", pipExtrasProject},
			[]string{"install", "--user", "-e", ".[dev]"}, selected, []string{"docdep"}},
		{".[DEV] normalizes the extra", []string{"pyproject.toml", pipExtrasProject},
			[]string{"install", ".[DEV]"}, selected, []string{"docdep"}},
		{".[dev] in a requirements file", []string{"pyproject.toml", pipExtrasProject, "requirements.txt", ".[dev]\n"},
			[]string{"install", "-r", "requirements.txt"}, selected, []string{"docdep"}},
		{"-e .[dev] in a requirements file", []string{"pyproject.toml", pipExtrasProject, "requirements.txt", "-e .[dev]\n"},
			[]string{"install", "-r", "requirements.txt"}, selected, []string{"docdep"}},
		{"./sub[docs] in a requirements file", []string{"sub/pyproject.toml", pipExtrasProject, "requirements.txt", "./sub[docs]\n"},
			[]string{"install", "-r", "requirements.txt"}, []string{"builddep", "rundep", "docdep"}, []string{"devdep"}},
		{"no extras selects no group", []string{"pyproject.toml", pipExtrasProject},
			[]string{"install", "-e", "."}, []string{"builddep", "rundep"}, []string{"devdep", "docdep"}},
		{"a self-reference's marker is joined to its group's", []string{"pyproject.toml",
			"[project]\nname = \"proj\"\nversion = \"0\"\n[project.optional-dependencies]\ndev = [\"proj[win] ; sys_platform == 'win32'\"]\nwin = [\"winonly\", \"both ; python_version > '3'\"]\n"},
			[]string{"install", "-e", ".[dev]"},
			[]string{"winonly ; sys_platform == 'win32'", "both ; (python_version > '3') and (sys_platform == 'win32')"}, []string{"winonly"}},
		{"an unconditional reference after a conditional one", []string{"pyproject.toml",
			"[project]\nname = \"proj\"\nversion = \"0\"\n[project.optional-dependencies]\ndev = [\"proj[win] ; sys_platform == 'win32'\", \"proj[win]\"]\nwin = [\"winonly\"]\n"},
			[]string{"install", "-e", ".[dev]"}, []string{"winonly ; sys_platform == 'win32'", "winonly"}, nil},
		{"inline optional-dependencies table", []string{"pyproject.toml",
			"[project]\nname = \"proj\"\nversion = \"0\"\noptional-dependencies = { dev = [\"devdep\"] }\n"},
			[]string{"install", "-e", ".[dev]"}, []string{"devdep"}, nil},
		{"dynamic optional dependencies are unknown", []string{"pyproject.toml",
			"[project]\nname = \"proj\"\nversion = \"0\"\ndynamic = [\"optional-dependencies\"]\n[project.optional-dependencies]\ndev = [\"devdep\"]\n"},
			[]string{"install", "-e", ".[dev]"}, []string{"setuptools>=40.8.0"}, []string{"devdep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := newNeedProject(t, tc.files...)
			fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: tc.args}, project.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if fetch == nil {
				t.Fatalf("fetch = nil, want a download of %q", tc.want)
			}
			for _, want := range tc.want {
				if !slices.Contains(fetch.Args, want) {
					t.Errorf("fetch = %q, want %q", fetch.Args, want)
				}
			}
			for _, forbid := range append(tc.forbid, "proj", "Proj", "proj[test]", "proj[dev]") {
				if slices.Contains(fetch.Args, forbid) {
					t.Errorf("fetch = %q must not carry %q", fetch.Args, forbid)
				}
			}
			forbidBuildableFetchTargets(t, fetch)
		})
	}
}

// A VCS or URL requirement in an optional-dependency group the extras select
// is a declared remote dependency the fetch never gets (an environment verdict
// when the offline install then fails); an unselected group does not count.
func TestPipUnfetchedRemoteTargetInExtras_GEC1021R5(t *testing.T) {
	project := newNeedProject(t, "pyproject.toml",
		"[project]\nname = \"proj\"\nversion = \"0\"\ndependencies = [\"six\"]\n[project.optional-dependencies]\ndev = [\"proj[vcs]\"]\nvcs = [\"pkg @ git+https://h.example/r.git\"]\n")
	if got, want := project.pipUnfetchedRemoteTarget(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-e", ".[dev]"}}),
		"pkg @ git+https://h.example/r.git (optional dependencies in pyproject.toml)"; got != want {
		t.Fatalf("pipUnfetchedRemoteTarget = %q, want %q", got, want)
	}
	if got := project.pipUnfetchedRemoteTarget(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-e", "."}}); got != "" {
		t.Fatalf("pipUnfetchedRemoteTarget without extras = %q, want none", got)
	}
}

// pipWheelMaker writes a minimal pure-Python wheel for each name given after
// the output directory.
//
//nolint:gosec // G101: a test script that writes a wheel's RECORD digests, not a credential.
const pipWheelMaker = `import base64, hashlib, os, sys, zipfile
out = sys.argv[1]
os.makedirs(out, exist_ok=True)
for name in sys.argv[2:]:
    dist = name + "-1.0"
    files = {
        name + "/__init__.py": "",
        dist + ".dist-info/METADATA": "Metadata-Version: 2.1\nName: " + name + "\nVersion: 1.0\n",
        dist + ".dist-info/WHEEL": "Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
    }
    record = []
    with zipfile.ZipFile(os.path.join(out, dist + "-py3-none-any.whl"), "w") as z:
        for path, data in files.items():
            b = data.encode()
            z.writestr(path, b)
            digest = base64.urlsafe_b64encode(hashlib.sha256(b).digest()).rstrip(b"=").decode()
            record.append(path + ",sha256=" + digest + "," + str(len(b)))
        record.append(dist + ".dist-info/RECORD,,")
        z.writestr(dist + ".dist-info/RECORD", "\n".join(record) + "\n")
`

// pipExtrasBackend is an in-tree PEP 517/660 backend for pipExtrasProject: its
// wheel metadata declares the same dependencies and extras.
const pipExtrasBackend = `import base64, hashlib, os, zipfile
DIST = "proj-0.1"
METADATA = """Metadata-Version: 2.1
Name: proj
Version: 0.1
Requires-Dist: rundep
Provides-Extra: dev
Requires-Dist: devdep; extra == "dev"
Requires-Dist: proj[test]; extra == "dev"
Provides-Extra: test
Requires-Dist: testdep; python_version >= "3" and extra == "test"
Requires-Dist: proj[dev]; extra == "test"
Provides-Extra: docs
Requires-Dist: docdep; extra == "docs"
"""
WHEEL = "Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"

def _wheel(directory, files):
    files[DIST + ".dist-info/METADATA"] = METADATA
    files[DIST + ".dist-info/WHEEL"] = WHEEL
    name = DIST + "-py3-none-any.whl"
    record = []
    with zipfile.ZipFile(os.path.join(directory, name), "w") as z:
        for path, data in files.items():
            b = data.encode()
            z.writestr(path, b)
            digest = base64.urlsafe_b64encode(hashlib.sha256(b).digest()).rstrip(b"=").decode()
            record.append(path + ",sha256=" + digest + "," + str(len(b)))
        record.append(DIST + ".dist-info/RECORD,,")
        z.writestr(DIST + ".dist-info/RECORD", "\n".join(record) + "\n")
    return name

def get_requires_for_build_wheel(config_settings=None):
    return []

def get_requires_for_build_editable(config_settings=None):
    return []

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    return _wheel(wheel_directory, {"proj/__init__.py": ""})

def build_editable(wheel_directory, config_settings=None, metadata_directory=None):
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    return _wheel(wheel_directory, {"proj.pth": root + "\n"})
`

// Live, with this host's python3 and pip: the offline install of `-e .[dev]`
// succeeds from the wheelhouse the fetch stage fills, because the fetch now
// carries the project's build requirement, its dependency and both groups the
// extra selects (dev, and test through the self-referencing cycle). The fetch
// is pointed at a local directory of wheels instead of an index, so nothing
// touches the network; the offline install runs exactly as the build stage
// does, in a throwaway virtualenv.
func TestPipEditableExtrasInstallOffline_GEC1021R5(t *testing.T) {
	if testing.Short() {
		t.Skip("live pip test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	work := t.TempDir()
	env := []string{"PATH=" + filepath.Join(work, "venv", "bin") + ":/usr/bin:/bin", "HOME=" + filepath.Join(work, "home"),
		"PIP_CONFIG_FILE=/dev/null", "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1", "PIP_NO_CACHE_DIR=1", "PYTHONNOUSERSITE=1"}
	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}
	if out, err := exec.Command(python, "-m", "venv", filepath.Join(work, "venv")).CombinedOutput(); err != nil {
		t.Skipf("python3 cannot create a virtualenv: %v\n%s", err, out)
	}
	index := filepath.Join(work, "index")
	run(work, filepath.Join(work, "venv", "bin", "python"), "-I", "-c", pipWheelMaker, index, "builddep", "rundep", "devdep", "testdep", "docdep")

	project := newNeedProject(t, "pyproject.toml", pipExtrasProject, "_backend/backend.py", pipExtrasBackend, "proj/__init__.py", "")
	fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-e", ".[dev]"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fetch == nil {
		t.Fatal("fetch = nil: the networked download receives nothing for the local project with extras")
	}
	pip := filepath.Join(work, "venv", "bin", "pip")
	run(project.Path, pip, append(slices.Clone(fetch.Args), "--no-index", "--find-links", index)...)
	run(project.Path, pip, build.Args...)
	run(work, filepath.Join(work, "venv", "bin", "python"), "-I", "-c", "import proj, rundep, devdep, testdep")
}

// registryLeftPadLock is a lockfile (v3) that takes its one package from the
// npm registry.
const registryLeftPadLock = `{
  "name": "x",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {"name": "x", "dependencies": {"left-pad": "^1.3.0"}},
    "node_modules/left-pad": {"version": "1.3.0", "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz", "integrity": "sha512-x"}
  }
}
`

// stubNpmVersion makes the networked npm fetch see an npm of this major
// version.
func stubNpmVersion(t *testing.T, major int) {
	t.Helper()
	old := npmVersionProbe
	npmVersionProbe = func(string) int { return major }
	t.Cleanup(func() { npmVersionProbe = old })
}

// Only a registry spec is fetched from the registry: npm-package-arg reads
// "~1.2.3" as a range, but https://github.com/user/repo as a git dependency
// (the round-2 check took every https spec for a scriptless tarball), and
// ssh:, sourcehut: and a local tarball name are not registry specs either.
func TestNpmNonRegistrySpec_GEC1021R5(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want bool
	}{
		{"~4.17.21", false}, // a tilde range, not a home-relative path
		{"", false},
		{"*", false},
		{">=1.0.0 <2 || ^3", false},
		{"next", false},
		{"npm:@scope/pkg@^1", false},
		{"NPM:left-pad", false},
		{"https://github.com/user/repo", true},
		{"https://gitlab.com/user/repo", true},
		{"ssh://git@github.com/user/repo", true},
		{"git@github.com:user/repo.git", true},
		{"sourcehut:~user/repo", true},
		{"gist:abc123", true},
		{"~/local", true},
		{"pkg.tgz", true},
		{"pkg-1.0.0.tar.gz", true},
		{"C:dep", true},
		{"npm:github:user/repo", true},
		{"npm:git+https://h.example/r.git", true},
		{"npm:left-pad@file:../x", true},
		{"npm:a@npm:b@1", true}, // nested aliases are not supported
	} {
		if got := npmNonRegistrySpec(tc.spec, false); got != tc.want {
			t.Errorf("npmNonRegistrySpec(%q) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

// The static check reads package.json (a workspaces root, every dependency and
// override spec) and every entry of the lockfiles npm follows: package-lock.json
// and npm-shrinkwrap.json of any lockfileVersion, and a v1 yarn.lock (npm
// 10.8.2 ran a git dependency's prepare when only a yarn.lock named it).
func TestNpmNonRegistryDependencyStatic_GEC1021R5(t *testing.T) {
	lock := func(entries string) string {
		return `{"lockfileVersion": 3, "packages": {"": {"name": "x"}, ` + entries + `}}`
	}
	const manifest = `{"name":"x","dependencies":{"left-pad":"^1.3.0"}}`
	for _, tc := range []struct {
		name  string
		files []string
		want  string // a substring of the reason; "" for none
	}{
		{"tilde range", []string{"package.json", `{"name":"x","dependencies":{"lodash":"~4.17.21"}}`}, ""},
		{"https GitHub repository", []string{"package.json", `{"name":"x","dependencies":{"dep":"https://github.com/user/repo"}}`}, "dep (dependencies: https://github.com/user/repo)"},
		{"workspaces root", []string{"package.json", `{"name":"x","workspaces":["packages/*"]}`}, "workspaces root"},
		{"yarn-style workspaces object", []string{"package.json", `{"name":"x","workspaces":{"packages":["p/*"]}}`}, "workspaces root"},
		{"empty workspaces", []string{"package.json", `{"name":"x","workspaces":[]}`}, ""},
		{"override with a git repository", []string{"package.json", `{"name":"x","overrides":{"a":{"b":"github:user/repo"},"c":"$c"}}`}, "b: github:user/repo"},
		{"registry overrides", []string{"package.json", `{"name":"x","overrides":{"a":"1.0.0","b":{".":"^2","c":"$c"}}}`}, ""},
		{"registry lockfile", []string{"package.json", manifest, "package-lock.json", registryLeftPadLock}, ""},
		{"scoped, aliased and mirrored registry tarballs", []string{"package.json", manifest, "package-lock.json", lock(
			`"node_modules/@s/p": {"version": "2.0.0", "resolved": "https://registry.npmjs.org/@s/p/-/p-2.0.0.tgz"},` +
				`"node_modules/pad": {"name": "left-pad", "version": "1.3.0", "resolved": "https://registry.npmmirror.com/left-pad/-/left-pad-1.3.0.tgz"},` +
				`"node_modules/b": {"version": "1.0.0", "resolved": "https://corp.example/artifactory/api/npm/npm/b/-/b-1.0.0.tgz"},` +
				`"node_modules/bundled": {"version": "1.0.0", "inBundle": true}`)}, ""},
		{"git entry", []string{"package.json", manifest, "package-lock.json", lock(`"node_modules/a": {"version": "1.0.0", "resolved": "git+ssh://git@github.com/u/r.git#0123"}`)}, "a from git+ssh://"},
		{"file entry", []string{"package.json", manifest, "package-lock.json", lock(`"node_modules/a": {"version": "1.0.0", "resolved": "file:../a.tgz"}`)}, "a from file:../a.tgz"},
		{"tarball outside the registry", []string{"package.json", manifest, "package-lock.json", lock(`"node_modules/a": {"version": "1.0.0", "resolved": "https://evil.example/a.tgz"}`)}, "a from the URL https://evil.example/a.tgz"},
		{"tarball of another version", []string{"package.json", manifest, "package-lock.json", lock(`"node_modules/a": {"version": "1.0.0", "resolved": "https://registry.npmjs.org/a/-/a-6.6.6.tgz"}`)}, "a from the URL"},
		{"link to a workspace", []string{"package.json", manifest, "package-lock.json", lock(`"node_modules/w": {"resolved": "packages/w", "link": true}`)}, "w as a link to packages/w"},
		{"local package", []string{"package.json", manifest, "package-lock.json", lock(`"../dirdep": {"version": "1.0.0"}`)}, "the local package ../dirdep"},
		{"v1 git version", []string{"package.json", manifest, "npm-shrinkwrap.json", `{"lockfileVersion": 1, "dependencies": {"a": {"version": "github:u/r#0123"}}}`}, "npm-shrinkwrap.json takes a from github:u/r#0123"},
		{"v1 nested URL", []string{"package.json", manifest, "package-lock.json", `{"lockfileVersion": 1, "dependencies": {"a": {"version": "1.0.0", "resolved": "https://registry.npmjs.org/a/-/a-1.0.0.tgz", "dependencies": {"b": {"version": "1.0.0", "resolved": "https://evil.example/b.tgz"}}}}}`}, "b from the URL"},
		{"v1 registry alias", []string{"package.json", manifest, "package-lock.json", `{"lockfileVersion": 1, "dependencies": {"pad": {"version": "npm:left-pad@1.3.0", "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}`}, ""},
		{"yarn.lock resolving a range to git", []string{"package.json", `{"name":"x","dependencies":{"gitdep":"^1.0.0"}}`, "yarn.lock",
			"# yarn lockfile v1\n\n\ngitdep@^1.0.0:\n  version \"1.0.0\"\n  resolved \"git+file:///tmp/gitdep#0123\"\n"}, "yarn.lock, whose resolutions npm follows without a package lock, takes gitdep from git+file:///tmp/gitdep#0123"},
		{"yarn.lock registry entries", []string{"package.json", manifest, "yarn.lock",
			"# yarn lockfile v1\n\n\n\"@babel/code-frame@^7.0.0\", \"@babel/code-frame@^7.10.4\":\n  version \"7.12.13\"\n  resolved \"https://registry.yarnpkg.com/@babel/code-frame/-/code-frame-7.12.13.tgz#dcfc826beef65e75c50e21d3837d7d95798dd658\"\n  dependencies:\n    version \"^1.0.0\"\n\npad@npm:left-pad@^1.3.0:\n  version \"1.3.0\"\n  resolved \"https://registry.yarnpkg.com/left-pad/-/left-pad-1.3.0.tgz#5b8a3a7765dfe001261dde915589e782f8c94d1e\"\n"}, ""},
		{"yarn.lock file dependency", []string{"package.json", manifest, "yarn.lock", "# yarn lockfile v1\n\n\n\"dep@file:../dep\":\n  version \"1.0.0\"\n"}, "dep (dep@file:../dep)"},
		{"berry yarn.lock is ignored, as npm ignores it", []string{"package.json", manifest, "yarn.lock",
			"__metadata:\n  version: 6\n\n\"gitdep@npm:^1.0.0\":\n  version: 1.0.0\n  resolution: \"gitdep@git+file:///tmp/gitdep#commit=0123\"\n"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := newNeedProject(t, tc.files...).npmNonRegistryDependency()
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("npmNonRegistryDependency = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("unreadable lockfiles are errors", func(t *testing.T) {
		for _, data := range []string{`{`, `{"packages": {"node_modules/a": {"version": 1}}}`, `{"dependencies": {"a": "1.0.0"}}`} {
			if _, err := npmLockfileNonRegistry([]byte(data)); err == nil {
				t.Errorf("npmLockfileNonRegistry(%s) = nil error, want one", data)
			}
		}
	})
}

// The npm version is read from the package.json of the npm install the
// trusted bin/npm a fetch runs links into, without running anything.
func TestNpmMajorVersion_GEC1021R5(t *testing.T) {
	install := func(t *testing.T, pkg string) string {
		t.Helper()
		prefix := t.TempDir()
		cli := filepath.Join(prefix, "lib", "node_modules", "npm", "bin", "npm-cli.js")
		writeExecScript(t, cli, "exit 0\n")
		writeFile(t, filepath.Join(prefix, "lib", "node_modules", "npm", "package.json"), pkg)
		if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../lib/node_modules/npm/bin/npm-cli.js", filepath.Join(prefix, "bin", "npm")); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(prefix, "bin")
	}
	for _, tc := range []struct {
		name string
		pkg  string
		want int
	}{
		{"npm 12", `{"name":"npm","version":"12.2.0"}`, 12},
		{"npm 10", `{"name":"npm","version":"10.8.2"}`, 10},
		{"not npm's package.json", `{"name":"evil","version":"99.0.0"}`, 0},
		{"unreadable version", `{"name":"npm","version":"x"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := install(t, tc.pkg)
			if got := npmMajorVersion(filepath.Join(bin, "npm")); got != tc.want {
				t.Fatalf("npmMajorVersion = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("a wrapper script is no install", func(t *testing.T) {
		bin := t.TempDir()
		writeExecScript(t, filepath.Join(bin, "npm"), "exit 0\n")
		if got := npmMajorVersion(filepath.Join(bin, "npm")); got != 0 {
			t.Fatalf("npmMajorVersion = %d, want 0", got)
		}
	})
	t.Run("a bare name is no install", func(t *testing.T) {
		if got := npmMajorVersion("npm"); got != 0 {
			t.Fatalf("npmMajorVersion = %d, want 0", got)
		}
	})
}

// fakeNpmForLockTest records every npm run (its arguments and whether the
// sandbox had a default route) in $FAKE_NPM_LOG. A lock resolution copies the
// fixture resolved-lock.json (beside the fake npm, outside the workspace) into
// the fetch directory, or reports the failure the fixture allowgit or
// (offline) notcached asks for; an install records whether a package-lock.json
// is in place and installs left-pad into node_modules.
const fakeNpmForLockTest = `log="$FAKE_NPM_LOG"
net=no
/bin/grep -q '	00000000	' /proc/net/route 2>/dev/null && net=yes
echo "$* net=$net" >> "$log"
lockonly=; offline=
for a in "$@"; do
  case "$a" in --package-lock-only) lockonly=1 ;; --offline) offline=1 ;; esac
done
fx="$(/usr/bin/dirname "$0")/fixtures"
if [ -n "$lockonly" ]; then
  if [ -f "$fx/allowgit" ]; then
    printf 'npm error code EALLOWGIT\nnpm error Fetching packages of type "git" have been disabled\nnpm error Refusing to fetch "gitdep@git+https://example.invalid/r.git"\n' >&2
    exit 1
  fi
  if [ -n "$offline" ] && [ -f "$fx/notcached" ]; then
    printf 'npm error code ENOTCACHED\nnpm error request to https://registry.npmjs.org/is-odd failed: cache mode is only-if-cached but no cached response is available.\n' >&2
    exit 1
  fi
  [ -f "$fx/resolved-lock.json" ] && /bin/cp "$fx/resolved-lock.json" package-lock.json
  exit 0
fi
[ -f package-lock.json ] && echo "install saw $(/bin/grep -c resolved package-lock.json) resolved" >> "$log"
/bin/mkdir -p node_modules/left-pad && echo '{"name":"left-pad","version":"1.3.0"}' > node_modules/left-pad/package.json
exit 0
`

// Live, in the real sandbox with slirp4netns and a fake npm outside the
// workspace: a networked npm fetch first resolves the tree into a lockfile in
// its fetch directory (with the network for npm 12 or later, offline from the
// project's lockfile before it), refuses one that takes a package from
// anywhere but the registry, and otherwise runs `npm ci` of exactly that
// lockfile; the host then puts node_modules into the workspace, and the
// lockfile too unless the plan writes none.
func TestNpmFetchResolvesThroughInspectedLockfile_GEC1021R5(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	gitLock := `{"lockfileVersion": 3, "packages": {"": {"name": "x"}, "node_modules/gitdep": {"version": "1.0.0", "resolved": "git+ssh://git@github.com/u/r.git#0123"}}}`
	for _, tc := range []struct {
		name     string
		major    int
		files    []string // project files
		fixtures []string // files beside the fake npm that steer it
		args     []string
		refused  string   // a substring of the refusal; "" when the fetch runs
		log      []string // substrings of the npm log, in order
		absent   []string // substrings the npm log must not hold
		lockLeft bool     // a package-lock.json remains in the project
	}{
		{name: "npm 12 resolves with the network, then installs the inspected lockfile",
			major: 12, fixtures: []string{"resolved-lock.json", registryLeftPadLock},
			args: []string{"install", "--ignore-scripts", "--no-package-lock"},
			log:  []string{"install --package-lock-only --ignore-scripts --no-audit --no-fund net=yes", "ci --ignore-scripts --no-audit --no-fund net=yes", "install saw 1 resolved"}},
		{name: "a resolved git dependency is refused before the install",
			major: 12, fixtures: []string{"resolved-lock.json", gitLock},
			args: []string{"install", "--ignore-scripts", "--no-package-lock"}, refused: "gitdep from git+ssh://",
			log: []string{"--package-lock-only"}, absent: []string{"ci "}},
		{name: "npm refusing a git dependency is reported",
			major: 12, fixtures: []string{"allowgit", ""},
			args: []string{"install", "--ignore-scripts"}, refused: "npm refused to fetch gitdep@git+https://example.invalid/r.git, a git dependency",
			absent: []string{"ci "}},
		{name: "npm before 12 without a lockfile is refused before npm runs",
			major: 10, args: []string{"install", "--ignore-scripts"}, refused: "no package-lock.json",
			absent: []string{"install", "ci "}},
		{name: "npm before 12 resolves offline from the project's lockfile",
			major: 10, files: []string{"package-lock.json", registryLeftPadLock},
			args: []string{"install", "--ignore-scripts"},
			log:  []string{"--package-lock-only --ignore-scripts --no-audit --no-fund --offline net=no", "ci --ignore-scripts --no-audit --no-fund net=yes", "install saw 1 resolved"}, lockLeft: true},
		{name: "npm ci resolves nothing",
			major: 10, files: []string{"package-lock.json", registryLeftPadLock},
			args: []string{"ci"},
			log:  []string{"ci --ignore-scripts --no-audit --no-fund net=yes", "install saw 1 resolved"}, absent: []string{"--package-lock-only"}, lockLeft: true},
		{name: "npm before 12 with a stale lockfile is refused",
			major: 10, files: []string{"package-lock.json", registryLeftPadLock}, fixtures: []string{"notcached", ""},
			args: []string{"install", "--ignore-scripts"}, refused: "does not hold every dependency",
			log: []string{"--offline net=no"}, absent: []string{"ci "}, lockLeft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := sandboxDepsProject(t)
			requireUserModeNetwork(t)
			stubNpmVersion(t, tc.major)
			tools := outsideTools(t)
			if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
				t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
			}
			writeExecScript(t, filepath.Join(tools, "npm"), fakeNpmForLockTest)
			writeProjectFiles(t, filepath.Join(tools, "fixtures"), append([]string{".keep", ""}, tc.fixtures...)...)
			writeProjectFiles(t, project.Path, append([]string{"package.json", `{"name":"x","dependencies":{"left-pad":"^1.3.0"}}`}, tc.files...)...)
			before, _ := project.ReadFile("package-lock.json")
			npmLog := fetchLogPath(t, "npm")

			result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: tc.args, env: []string{"FAKE_NPM_LOG=" + npmLog}})
			if tc.refused != "" {
				if !errorIsDepNetworkUnavailable(err) || !strings.Contains(noteOf(result), tc.refused) {
					t.Fatalf("err = %v, note = %q, want a refusal naming %q", err, noteOf(result), tc.refused)
				}
			} else if err != nil || result == nil || result.ExitCode != 0 {
				t.Fatalf("result = %+v, err = %v, want the fetch to succeed", result, err)
			}
			log := readLog(t, npmLog)
			rest := log
			for _, want := range tc.log {
				i := strings.Index(rest, want)
				if i < 0 {
					t.Fatalf("npm log lacks %q (in order); log:\n%s", want, log)
				}
				rest = rest[i+len(want):]
			}
			for _, absent := range tc.absent {
				if strings.Contains(log, absent) {
					t.Fatalf("npm log must not hold %q; log:\n%s", absent, log)
				}
			}
			after, err := project.ReadFile("package-lock.json")
			if tc.lockLeft != (err == nil) || tc.lockLeft && after != before {
				t.Fatalf("package-lock.json after the fetch: %q (err %v), want it left as before: %v", after, err, tc.lockLeft)
			}
			_, installed := project.ReadFile(filepath.Join("node_modules", "left-pad", "package.json"))
			if (tc.refused == "") != (installed == nil) {
				t.Fatalf("node_modules/left-pad in the project: %v, want it placed exactly when the fetch ran", installed == nil)
			}
		})
	}
}

// Live, with this host's real npm in the real sandbox and slirp4netns: the two
// npm stages accept makewand's command lines, and a verification-style install
// (--no-package-lock) leaves no lockfile behind. The project has no
// dependencies and audit, fund and update checks are off, so nothing is
// downloaded.
func TestNpmFetchRealNpmLive_GEC1021R5(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("no npm on PATH")
	}
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	t.Setenv("PATH", filepath.Dir(npm)+":/usr/bin:/bin")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	writeProjectFiles(t, project.Path, "package.json", `{"name":"x","version":"1.0.0"}`)
	plan := ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts", "--no-package-lock"},
		env: []string{"npm_config_audit=false", "npm_config_fund=false", "npm_config_update_notifier=false"}}
	result, err := project.RunVerificationPlan(context.Background(), plan)
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v, want the npm %d fetch to succeed", result, err, npmMajorVersion(npm))
	}
	if _, err := project.ReadFile("package-lock.json"); err == nil {
		t.Fatal("a --no-package-lock fetch must leave no package-lock.json behind")
	}
}

// Live, with an npm before 12 on this host (npm 10.8.2 at /usr/bin) in the real
// sandbox with slirp4netns: two projects whose package.json names no git or
// local dependency still made the networked `npm install --ignore-scripts` run
// project code - a yarn.lock that resolves a registry range to a git
// repository (npm follows it without a package lock) and a workspaces root
// (npm runs each workspace's prepare). Both are now refused before npm runs.
// The git repository is a bare one in the workspace (the fetch masks only
// directories named .git), so nothing is fetched from the network.
func TestNpmPrepareVectorsRefusedLive_GEC1021R5(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	const npmBin = "/usr/bin/npm"
	if _, err := os.Stat(npmBin); err != nil {
		t.Skip("no npm at /usr/bin")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	open := func(t *testing.T) (*Project, string, string) {
		t.Helper()
		project := sandboxDepsProject(t)
		requireUserModeNetwork(t)
		t.Setenv("PATH", "/usr/bin:/bin")
		if major := npmMajorVersion(npmBin); major == 0 || major >= npmNetworkedLockMajor {
			t.Skipf("%s is npm %d, not an npm before %d", npmBin, major, npmNetworkedLockMajor)
		}
		if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
			t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
		}
		marker := filepath.Join(project.Path, "PREPARED")
		return project, marker, `"scripts":{"prepare":"echo ran >> ` + marker + `"}`
	}
	gitRun := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	plan := ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"},
		env: []string{"npm_config_audit=false", "npm_config_fund=false", "npm_config_update_notifier=false"}}

	t.Run("yarn.lock resolving a range to git", func(t *testing.T) {
		project, marker, scripts := open(t)
		src := t.TempDir()
		writeFile(t, filepath.Join(src, "package.json"), `{"name":"gitdep","version":"1.0.0",`+scripts+`}`)
		gitRun(t, src, "init", "-q")
		gitRun(t, src, "add", "package.json")
		gitRun(t, src, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-qm", "init")
		bare := filepath.Join(project.Path, "gitdep.git")
		gitRun(t, src, "clone", "-q", "--bare", src, bare)
		sha := gitRun(t, bare, "rev-parse", "HEAD")
		writeProjectFiles(t, project.Path,
			"package.json", `{"name":"x","version":"1.0.0","dependencies":{"gitdep":"^1.0.0"}}`,
			"yarn.lock", "# yarn lockfile v1\n\n\ngitdep@^1.0.0:\n  version \"1.0.0\"\n  resolved \"git+file://"+bare+"#"+sha+"\"\n")
		_, err := project.RunVerificationPlan(context.Background(), plan)
		if data, _ := os.ReadFile(marker); len(data) > 0 {
			t.Fatalf("the git dependency's prepare ran during the networked fetch (err %v)", err)
		}
		if !errorIsDepNetworkUnavailable(err) || !strings.Contains(err.Error(), "yarn.lock") {
			t.Fatalf("err = %v, want the fetch refused for the yarn.lock git resolution", err)
		}
	})
	t.Run("workspaces root", func(t *testing.T) {
		project, marker, scripts := open(t)
		writeProjectFiles(t, project.Path,
			"package.json", `{"name":"x","version":"1.0.0","workspaces":["packages/*"]}`,
			"packages/a/package.json", `{"name":"a","version":"1.0.0",`+scripts+`}`)
		_, err := project.RunVerificationPlan(context.Background(), plan)
		if data, _ := os.ReadFile(marker); len(data) > 0 {
			t.Fatalf("the workspace's prepare ran during the networked fetch (err %v)", err)
		}
		if !errorIsDepNetworkUnavailable(err) || !strings.Contains(err.Error(), "workspaces root") {
			t.Fatalf("err = %v, want the fetch refused for the workspaces root", err)
		}
	})
}
