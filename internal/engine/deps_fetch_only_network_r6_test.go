//go:build linux

package engine

import (
	"archive/zip"
	"context"
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
// wheel target.
func TestWheelRequiresDistFetched_GEC1021R6(t *testing.T) {
	project := newNeedProject(t)
	writeTestWheel(t, filepath.Join(project.Path, "dist", "x-1.0-py3-none-any.whl"), "x-1.0.dist-info",
		"Metadata-Version: 2.1\nName: x\nVersion: 1.0\n"+
			"Requires-Dist: requests>=2\n"+
			"Requires-Dist: colorama ; sys_platform == \"win32\"\n"+
			"Requires-Dist: pytest>=7 ; extra == \"feature\"\n"+
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
	if !hasPrefix(fetch.Args, "pytest>=7") {
		t.Fatalf("fetch = %v, want the selected extra's dependency pytest", fetch.Args)
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
// body; the extra-marker rewrite selects what pip would.
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

	sel := map[string]bool{"dev": true}
	for _, tc := range []struct {
		in   string
		out  string
		keep bool
	}{
		{`pytest ; extra == "dev"`, `pytest ; "1" == "1"`, true},
		{`pytest ; extra == "docs"`, "", false},
		{`numpy ; python_version < "3.9"`, `numpy ; python_version < "3.9"`, true},
		{`requests>=2`, "requests>=2", true},
	} {
		out, keep := pipRewriteExtraMarker(tc.in, sel)
		if keep != tc.keep || (keep && out != tc.out) {
			t.Fatalf("pipRewriteExtraMarker(%q) = (%q, %v), want (%q, %v)", tc.in, out, keep, tc.out, tc.keep)
		}
	}
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
