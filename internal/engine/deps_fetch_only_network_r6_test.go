//go:build linux

package engine

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GEC-1021-1 round 4: a networked dependency fetch reads no tool config an
// earlier loopback step wrote (item 1), host-side writes into sandbox paths do
// not follow a planted symlink (item 2), and a local wheel's Requires-Dist are
// fetched from its METADATA statically (item 3).

// Item 1, end to end: a git credential helper a loopback step wrote into the
// persistent sandbox HOME never runs during the networked fetch, and the fetch
// runs with the fresh HOME and the neutralised config-file variables. Fails on
// the base, where the fetch runs with the persistent sandbox HOME.
func TestNetworkedFetchFreshHomeNeutralizesGitCredentialHelper_GEC1021R6(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	requireUserModeNetwork(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	project, err := NewProject("fetch-only", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(project.Path, "tools")
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	// A loopback step wrote a git credential helper into the persistent sandbox
	// HOME; a later networked go/cargo-git fetch would run it on an HTTP 401.
	sandboxHome := filepath.Join(project.Path, filepath.FromSlash(sandboxHomeRelPath))
	if err := os.MkdirAll(sandboxHome, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(project.Path, "helper-ran")
	gitconfig := "[credential]\n\thelper = \"!sh -c 'touch " + marker + "'\"\n"
	if err := os.WriteFile(filepath.Join(sandboxHome, ".gitconfig"), []byte(gitconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fake `go mod download` that triggers a git credential lookup (reading
	// the sandbox's git config) and records its HOME and config-file variables.
	// The probe path (`go env`) does nothing, so it leaves no marker.
	const fakeGo = `if [ "$1" = mod ]; then
  printf 'protocol=https\nhost=example.com\n\n' | git credential fill >/dev/null 2>&1 || true
  {
    echo "HOME=$HOME"
    echo "GIT_CONFIG_GLOBAL=$GIT_CONFIG_GLOBAL"
    echo "GIT_CONFIG_NOSYSTEM=$GIT_CONFIG_NOSYSTEM"
    echo "PIP_CONFIG_FILE=$PIP_CONFIG_FILE"
    echo "GOENV=$GOENV"
    echo "NPM_CONFIG_USERCONFIG=$NPM_CONFIG_USERCONFIG"
  } > "$PWD/fetch-env.txt"
fi
exit 0
`
	writeExecScript(t, filepath.Join(tools, "go"), fakeGo)
	if _, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "download"}}); err != nil {
		t.Fatalf("RunVerificationPlan: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the git credential helper ran during the networked fetch; HOME was not fresh")
	}
	env := readLog(t, filepath.Join(project.Path, "fetch-env.txt"))
	for _, want := range []string{
		"HOME=" + fetchSandboxHome,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"PIP_CONFIG_FILE=" + os.DevNull,
		"GOENV=off",
		"NPM_CONFIG_USERCONFIG=" + fetchSandboxHome + "/.npmrc",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("networked fetch env missing %q; got:\n%s", want, env)
		}
	}
}

// Item 1, command construction: a networked fetch is wrapped with the fresh
// HOME and config-file variables; a loopback step is not.
func TestNetworkedFetchHomeArgs_GEC1021R6(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not on PATH")
	}
	ws := t.TempDir()
	setenv := func(args []string, key string) (string, bool) {
		for i := 0; i+2 < len(args); i++ {
			if args[i] == "--setenv" && args[i+1] == key {
				return args[i+2], true
			}
		}
		return "", false
	}
	_, fetchArgs, err := wrapVerificationCommand(bwrap, ws, "go", []string{"mod", "download"}, true, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if home, _ := setenv(fetchArgs, "HOME"); home != fetchSandboxHome {
		t.Fatalf("networked fetch HOME = %q, want the fresh %q", home, fetchSandboxHome)
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_GLOBAL":     os.DevNull,
		"GIT_CONFIG_NOSYSTEM":   "1",
		"PIP_CONFIG_FILE":       os.DevNull,
		"GOENV":                 "off",
		"NPM_CONFIG_USERCONFIG": fetchSandboxHome + "/.npmrc",
		"XDG_CONFIG_HOME":       fetchSandboxHome + "/.config",
	} {
		if got, ok := setenv(fetchArgs, key); !ok || got != want {
			t.Fatalf("networked fetch %s = %q (set %v), want %q", key, got, ok, want)
		}
	}
	if !slices.Contains(fetchArgs, "--dir") {
		t.Fatal("networked fetch does not create its fresh HOME directory")
	}

	_, loopArgs, err := wrapVerificationCommand(bwrap, ws, "go", []string{"test"}, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	sandboxHome := filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath))
	if home, _ := setenv(loopArgs, "HOME"); home != sandboxHome {
		t.Fatalf("loopback step HOME = %q, want the persistent %q", home, sandboxHome)
	}
	if _, ok := setenv(loopArgs, "GIT_CONFIG_GLOBAL"); ok {
		t.Fatal("loopback step neutralises git config; want it left alone")
	}
}

// Item 2: prepareSandboxHome refuses a .makewand replaced with a symlink
// pointing outside the workspace, rather than creating the sandbox HOME (and
// the go telemetry file) through it. Fails on the base, where MkdirAll follows
// the symlink.
func TestPrepareSandboxHomeRefusesSymlink_GEC1021R6(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, ".makewand")); err != nil {
		t.Fatal(err)
	}
	p := &Project{Path: ws}
	if err := p.prepareSandboxHome(ws); err == nil {
		t.Fatal("prepareSandboxHome followed the .makewand symlink; want a refusal")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote through the symlink into %s: %v", outside, entries)
	}
}

// Item 2: disableGoTelemetry refuses a .config replaced with a symlink pointing
// outside the sandbox HOME. Fails on the base, where MkdirAll follows it.
func TestDisableGoTelemetryRefusesSymlink_GEC1021R6(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".config")); err != nil {
		t.Fatal(err)
	}
	if err := disableGoTelemetry(home); err == nil {
		t.Fatal("disableGoTelemetry followed the .config symlink; want a refusal")
	}
	if _, err := os.Stat(filepath.Join(outside, "go", "telemetry", "mode")); err == nil {
		t.Fatalf("wrote the telemetry mode file through the symlink into %s", outside)
	}
}

// Item 2: the fetch scratch directory refuses a symlinked .makewand, and a
// host-side write into it never follows a symlink out of it.
func TestFetchScratchRefusesSymlinks_GEC1021R6(t *testing.T) {
	t.Run("symlinked .makewand", func(t *testing.T) {
		ws := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(ws, ".makewand")); err != nil {
			t.Fatal(err)
		}
		p := &Project{Path: ws}
		if _, _, err := p.newFetchScratch(ws); err == nil {
			t.Fatal("newFetchScratch used a symlinked .makewand; want a refusal")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("created the fetch directory through the symlink into %s: %v", outside, entries)
		}
	})

	t.Run("escaping symlink in the scratch directory", func(t *testing.T) {
		scratch := t.TempDir()
		outside := t.TempDir()
		victim := filepath.Join(outside, "victim.txt")
		if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(scratch, "evil")); err != nil {
			t.Fatal(err)
		}
		if _, err := writeFetchFile(scratch, "evil", "overwritten"); err == nil {
			t.Fatal("writeFetchFile wrote through the escaping symlink; want a refusal")
		}
		if data, _ := os.ReadFile(victim); string(data) != "original" {
			t.Fatalf("victim file overwritten through the symlink: %q", data)
		}
	})
}

// Item 2: the host-side writes (wheelhouse, constraints file) land under a
// fresh per-run directory, never inside the loopback-writable persistent
// sandbox HOME. Fails on the base, which writes them inside it.
func TestFetchArtifactsOutsideSandboxHome_GEC1021R6(t *testing.T) {
	project := newNeedProject(t, "requirements.txt", "flask==2.0\n", "constraints.txt", "flask==2.0\n")
	fetch, _, cleanup, err := project.planDependencyStages(
		ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-r", "requirements.txt", "-c", "constraints.txt"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	sandboxHome := filepath.Join(project.Path, filepath.FromSlash(sandboxHomeRelPath))
	for i, a := range fetch.Args {
		if (a == "--dest" || a == "--constraint") && i+1 < len(fetch.Args) {
			if strings.HasPrefix(fetch.Args[i+1], sandboxHome+string(filepath.Separator)) {
				t.Fatalf("%s %q is inside the persistent sandbox HOME; want a fresh per-run directory", a, fetch.Args[i+1])
			}
			if !strings.Contains(fetch.Args[i+1], filepath.FromSlash(".makewand/fetch-")) {
				t.Fatalf("%s %q is not under a fresh .makewand/fetch-* directory", a, fetch.Args[i+1])
			}
		}
	}
}

// Item 3: a local wheel's Requires-Dist are read from its METADATA and fetched,
// honouring the selected extras. Fails on the base, which fetches nothing for a
// wheel target, and on the round-4 commit, which handed pip a comparison of two
// strings for each extra clause and dropped parenthesized specifiers.
func TestWheelRequiresDistFetched_GEC1021R6(t *testing.T) {
	project := newNeedProject(t)
	writeTestWheel(t, filepath.Join(project.Path, "dist", "x-1.0-py3-none-any.whl"), "x-1.0.dist-info",
		"Metadata-Version: 2.1\nName: x\nVersion: 1.0\n"+
			"Provides-Extra: feature\nProvides-Extra: docs\n"+
			"Requires-Dist: requests>=2\n"+
			"Requires-Dist: wrapt (<2,>=1.10)\n"+
			"Requires-Dist: colorama ; sys_platform == \"win32\"\n"+
			"Requires-Dist: pytest>=7 ; extra == \"feature\"\n"+
			"Requires-Dist: importlib-metadata ; python_version < \"3.8\" and extra == \"feature\"\n"+
			"Requires-Dist: sphinx ; extra == \"docs\"\n"+
			"\nThe long description.\n")
	fetch, build, cleanup, err := project.planDependencyStages(
		ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "./dist/x-1.0-py3-none-any.whl[feature]"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fetch == nil {
		t.Fatal("no networked fetch for the wheel's Requires-Dist")
	}
	hasPrefix := func(args []string, prefix string) bool {
		return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, prefix) })
	}
	if !slices.Contains(fetch.Args, "requests>=2") {
		t.Fatalf("fetch = %v, want the wheel's base dependency requests>=2", fetch.Args)
	}
	// The selected extra's clause is decided by makewand, so pip gets the bare
	// requirement; the environment marker stays for pip to evaluate.
	if !slices.Contains(fetch.Args, "pytest>=7") {
		t.Fatalf("fetch = %v, want the selected extra's dependency as pytest>=7", fetch.Args)
	}
	if !slices.Contains(fetch.Args, `colorama ; sys_platform == "win32"`) {
		t.Fatalf("fetch = %v, want colorama with its environment marker", fetch.Args)
	}
	if !slices.Contains(fetch.Args, `importlib-metadata ; python_version < "3.8"`) {
		t.Fatalf("fetch = %v, want importlib-metadata with only its environment clause", fetch.Args)
	}
	if !slices.Contains(fetch.Args, "wrapt<2,>=1.10") {
		t.Fatalf("fetch = %v, want the parenthesized specifier as wrapt<2,>=1.10", fetch.Args)
	}
	for _, a := range fetch.Args {
		if !strings.HasPrefix(a, "-") && !filepath.IsAbs(a) && (strings.Contains(a, "extra") || strings.Contains(a, `"1" ==`)) {
			t.Fatalf("fetch = %v: %q leaves pip an extra clause it cannot decide or evaluate", fetch.Args, a)
		}
	}
	if hasPrefix(fetch.Args, "sphinx") {
		t.Fatalf("fetch = %v, must not fetch the unselected extra's dependency sphinx", fetch.Args)
	}
	for _, a := range fetch.Args {
		if strings.HasSuffix(a, ".whl") {
			t.Fatalf("fetch = %v, must not pass the wheel itself to the download", fetch.Args)
		}
	}
	if build == nil || !slices.Contains(build.Args, "./dist/x-1.0-py3-none-any.whl[feature]") {
		t.Fatalf("build = %v, want the offline install to keep the wheel target", build)
	}
}

// Item 3, unit: the METADATA header reader joins folded lines and stops at the
// body; a Requires-Dist marker's extra comparisons are decided by makewand and
// only environment clauses are left for pip, in the shapes real wheels write
// (setuptools, bdist_wheel, poetry-core, hatchling). The round-4 rewrite
// replaced each extra comparison with a comparison of two strings, which pip
// cannot evaluate (confirmed on pip 24 and 26): `pytest ; "1" == "1"`.
func TestWheelMetadataAndExtraMarker_GEC1021R6(t *testing.T) {
	md := "Metadata-Version: 2.1\nName: x\n" +
		"Requires-Dist: a>=1\n" +
		"Requires-Dist: b ;\n extra == \"dev\"\n" +
		"Requires-Dist: c\n\nRequires-Dist: not-a-header\n"
	got := wheelMetadataField(md, "Requires-Dist")
	want := []string{"a>=1", "b ; extra == \"dev\"", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("Requires-Dist = %v, want %v", got, want)
	}

	contexts := []string{"", "dev", "test"} // no extra, then the selected extras
	deep := strings.Repeat("(", 40) + `extra == "dev"` + strings.Repeat(")", 40)
	for _, tc := range []struct {
		in, out string // out "" = not fetched
	}{
		{`requests>=2`, "requests>=2"},
		{`pytest ; extra == "dev"`, "pytest"},
		{`pytest ; extra == "docs"`, ""},
		{`pytest;extra=='dev'`, "pytest"},
		{`numpy ; python_version < "3.9"`, `numpy ; python_version < "3.9"`},
		{`a ; python_version < "3.8" and extra == "dev"`, `a ; python_version < "3.8"`},
		{`a ; (python_version < "3.8") and extra == 'test'`, `a ; python_version < "3.8"`},
		{`a ; sys_platform == "win32" and extra == "docs"`, ""},
		{`a ; (python_version >= "3.8" and python_version < "4") and (extra == "dev")`, `a ; python_version >= "3.8" and python_version < "4"`},
		{`a ; (sys_platform == "linux" and platform_machine == "x86_64" or sys_platform == "win32" and platform_machine == "AMD64") and extra == "dev"`,
			`a ; (sys_platform == "linux" and platform_machine == "x86_64") or (sys_platform == "win32" and platform_machine == "AMD64")`},
		{`a ; extra == "docs" or extra == "dev"`, "a"},
		{`a ; extra == "Dev"`, "a"},         // names compare normalized (PEP 685)
		{`a ; "dev" == extra`, "a"},         // either side
		{`a ; extra != "dev"`, "a"},         // holds with no extra selected
		{`a ; extra in "dev docs"`, "a"},    // "" is in every string, as in Python
		{`a ; extra not in "dev test"`, ""}, // and so never not in one
		{`a ; extra not in "docs"`, "a"},
		{`a ; python_version >= "3" and (extra == "dev" or extra == "docs")`, `a ; python_version >= "3"`},
		{`a ; (extra == "dev" and python_version < "3.9") or (extra == "test" and sys_platform == "linux")`,
			`a ; python_version < "3.9" or sys_platform == "linux"`},
		{`a ; (extra == "dev" and (os_name == "nt" or python_version < "3.9")) or (extra == "test" and sys_platform == "linux")`,
			`a ; (os_name == "nt" or python_version < "3.9") or sys_platform == "linux"`},
		{`a ; os.name == "nt" and extra == "dev"`, `a ; os.name == "nt"`}, // a legacy variable spelling
		// The PEP 508 parenthesized specifiers bdist_wheel and poetry-core write.
		{`wrapt (<2,>=1.10)`, "wrapt<2,>=1.10"},
		{`cachecontrol[filecache] (>=0.13.0,<0.14.0) ; extra == "dev"`, "cachecontrol[filecache]>=0.13.0,<0.14.0"},
		{`PyQt5-sip ( >=12.11, <13)`, "PyQt5-sip>=12.11, <13"},
		{`foo (1.0)`, "foo (1.0)"}, // a bare version never joins the name (foo1.0); the strict parser drops it
		// Kept as written for pip: an extra comparison makewand does not decide,
		// a marker it cannot read, nesting deeper than it parses.
		{`a ; extra === "dev"`, `a ; extra === "dev"`},
		{`a ; extra == os_name`, `a ; extra == os_name`},
		{`a ; extras == "dev"`, `a ; extras == "dev"`},
		{`a ; extra == "dev`, `a ; extra == "dev`},
		{"a ; " + deep, "a ; " + deep},
	} {
		out, ok := pipWheelRequirement(tc.in, contexts)
		if ok != (tc.out != "") || out != tc.out {
			t.Errorf("pipWheelRequirement(%q) = (%q, %v), want %q", tc.in, out, ok, tc.out)
		}
		if strings.Contains(out, `"1" ==`) {
			t.Errorf("pipWheelRequirement(%q) = %q compares two strings", tc.in, out)
		}
	}
	// With no extra selected, only the no-extra context applies.
	if out, ok := pipWheelRequirement(`pytest ; extra == "dev"`, []string{""}); ok {
		t.Errorf("pipWheelRequirement without extras = %q, want not fetched", out)
	}
	if text, ok := pipNamedRequirementText("foo (1.0)"); ok {
		t.Errorf("strict parser took %q", text)
	}
}

// Item 3, fuzz: the marker parser never panics on an untrusted Requires-Dist
// marker, and what deciding its extra clauses leaves for pip - per extra
// context, and their "or" across contexts - has no extra clause and parses
// back to itself.
func FuzzPipMarkerExtraDecision_GEC1021R6(f *testing.F) {
	for _, seed := range []string{
		`extra == "dev"`,
		`(sys_platform == "linux" and platform_machine == "x86_64" or sys_platform == "win32") and extra == 'dev'`,
		`(extra == "dev" and python_version < "3.9") or (extra == "test" and os.name == "nt")`,
		`extra in "dev docs" and extra not in "test"`,
		`((((extra == "dev"))))`,
		`extra === "dev" or extra == os_name`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		m, ok := parsePipMarker(text)
		if !ok {
			return
		}
		var residuals []*pipMarker
		for _, extra := range []string{"", "dev", "test"} {
			residual, _, ok := m.withExtra(extra)
			if !ok {
				return // pip gets the marker as written
			}
			if residual != nil {
				residuals = append(residuals, residual)
			}
		}
		if len(residuals) > 1 {
			residuals = append(residuals, &pipMarker{op: "or", terms: slices.Clone(residuals)})
		}
		for _, residual := range residuals {
			left := residual.String()
			if back, ok := parsePipMarker(left); !ok || back.mentionsExtra() || back.String() != left {
				t.Fatalf("marker %q leaves %q for pip, which does not parse back without an extra clause", text, left)
			}
		}
	})
}

// Item 3, live with this host's python3 and pip: the download makewand builds
// for a local wheel with extras runs under real pip and fetches exactly what
// installing the wheel needs - its base requirement, one with parenthesized
// specifiers, and the selected extra's requirements whose environment clauses
// hold, but not an unselected or undeclared extra's, nor one whose environment
// clause fails - and the offline install then succeeds from the wheelhouse.
// Fails on the round-4 commit, whose download pip aborts on the rewritten
// marker (a comparison of two strings). Extra names are spelled as declared:
// pip before 24.1 compares them as written. The fetch is pointed at a local
// directory of wheels instead of an index, so nothing touches the network.
func TestWheelRequiresDistDownloadsUnderPip_GEC1021R6(t *testing.T) {
	if testing.Short() {
		t.Skip("live pip test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	work := t.TempDir()
	venvBin := filepath.Join(work, "venv", "bin")
	env := []string{"PATH=" + venvBin + ":/usr/bin:/bin", "HOME=" + filepath.Join(work, "home"),
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
	for _, name := range []string{"basedep", "pinned", "featdep", "envdep", "anydep", "windep", "nowin", "docdep", "undecl"} {
		writeInstallableWheel(t, index, name, "")
	}
	project := newNeedProject(t)
	writeInstallableWheel(t, filepath.Join(project.Path, "dist"), "x",
		"Provides-Extra: feature\nProvides-Extra: docs\n"+
			"Requires-Dist: basedep\n"+
			"Requires-Dist: pinned (>=1.0)\n"+
			"Requires-Dist: windep ; sys_platform == \"win32\"\n"+
			"Requires-Dist: featdep>=1 ; extra == \"feature\"\n"+
			"Requires-Dist: envdep ; python_version >= \"3\" and extra == 'feature'\n"+
			"Requires-Dist: nowin ; sys_platform == \"win32\" and extra == \"feature\"\n"+
			"Requires-Dist: anydep ; (sys_platform == \"win32\" or sys_platform != \"win32\") and (extra == \"docs\" or extra == \"feature\")\n"+
			"Requires-Dist: docdep ; extra == \"docs\"\n"+
			"Requires-Dist: undecl ; extra == \"undeclared\"\n")
	target := "./dist/x-1.0-py3-none-any.whl[feature,undeclared]"
	fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", target}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fetch == nil || build == nil {
		t.Fatalf("fetch = %v, build = %v: want both stages", fetch, build)
	}
	dest := slices.Index(fetch.Args, "--dest")
	if dest < 0 || dest+1 == len(fetch.Args) {
		t.Fatalf("fetch = %q has no --dest", fetch.Args)
	}
	pip := filepath.Join(venvBin, "pip")
	run(project.Path, pip, append(slices.Clone(fetch.Args), "--no-index", "--find-links", index)...)
	entries, err := os.ReadDir(fetch.Args[dest+1])
	if err != nil {
		t.Fatal(err)
	}
	var fetched []string
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry.Name(), "-")
		fetched = append(fetched, name)
	}
	slices.Sort(fetched)
	if want := []string{"anydep", "basedep", "envdep", "featdep", "pinned"}; !slices.Equal(fetched, want) {
		t.Fatalf("wheelhouse = %v, want %v (fetch %q)", fetched, want, fetch.Args)
	}
	run(project.Path, pip, build.Args...)
	run(work, filepath.Join(venvBin, "python"), "-I", "-c", "import x, basedep, pinned, featdep, envdep, anydep")
}

// writeTestWheel writes a minimal wheel (a zip holding one dist-info METADATA).
func writeTestWheel(t *testing.T, path, distInfo, metadata string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create(distInfo + "/METADATA")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, metadata); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeInstallableWheel writes dir/<name>-1.0-py3-none-any.whl, a pure-Python
// wheel pip installs: the package, METADATA with headers appended, WHEEL and
// RECORD.
func writeInstallableWheel(t *testing.T, dir, name, headers string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dist := name + "-1.0"
	f, err := os.Create(filepath.Join(dir, dist+"-py3-none-any.whl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	var record strings.Builder
	for _, file := range [][2]string{
		{name + "/__init__.py", ""},
		{dist + ".dist-info/METADATA", "Metadata-Version: 2.1\nName: " + name + "\nVersion: 1.0\n" + headers},
		{dist + ".dist-info/WHEEL", "Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"},
	} {
		w, err := zw.Create(file[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, file[1]); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(file[1]))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", file[0], base64.RawURLEncoding.EncodeToString(sum[:]), len(file[1]))
	}
	fmt.Fprintf(&record, "%s.dist-info/RECORD,,\n", dist)
	w, err := zw.Create(dist + ".dist-info/RECORD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, record.String()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
