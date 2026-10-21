//go:build linux

package engine

import (
	"context"
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
	if got := hostRustupHome("/home/u", env("rel/rustup")); got != "/home/u/.rustup" {
		t.Fatalf("relative RUSTUP_HOME: %q, want the HOME default", got)
	}
	if got := hostRustupHome("/home/u", env("/opt/rustup/")); got != "/opt/rustup" {
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
		tools := filepath.Join(project.Path, "tools")
		home := filepath.Join(project.Path, filepath.FromSlash(sandboxHomeRelPath))
		for _, name := range []string{"cargo", "rustc"} {
			writeExecScript(t, filepath.Join(tools, name), `echo "`+name+` RUSTUP_TOOLCHAIN=$RUSTUP_TOOLCHAIN" >> "$HOME/toolchain.log"
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
		return project, filepath.Join(home, "toolchain.log")
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
