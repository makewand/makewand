//go:build linux

package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// GEC-1021-1 round 7 (the structural fix of PR #91): a networked dependency
// fetch runs in a fresh, manifest-only fetch directory outside the workspace
// and the sandbox HOME, with trusted tools and a PATH outside the workspace;
// what it downloads lands in host-owned caches that every later loopback step
// reads read-only; host-side writes go through os.Root handles. Each finding
// of the round-4 review has a regression test here that fails on adfdf17.

// outsideTools makes a directory for fake tools outside the workspace - under
// the test's own HOME, which the sandbox re-binds read-only like any toolchain
// directory there - and puts it first on PATH.
func outsideTools(t *testing.T) string {
	t.Helper()
	home := os.Getenv("HOME")
	if home == "" || !strings.HasPrefix(home, os.TempDir()) {
		t.Fatalf("outsideTools needs the test's own HOME, got %q", home)
	}
	tools := filepath.Join(home, "tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	return tools
}

// fetchLogPath returns a file the fake tools of a test can log to from inside a
// networked fetch: the shared dependency caches are the only place a fetch
// sandbox writes that outlives it. Loopback steps see them read-only.
func fetchLogPath(t *testing.T, name string) string {
	t.Helper()
	base, err := depFetchBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "cache", "test-logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+"-"+rand.Text()+".log")
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// liveSandbox skips a live test without bubblewrap isolation or the user-mode
// network helper.
func liveSandbox(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
}

// boundPaths returns every host path a bwrap argv binds (sources and targets
// of --bind, --ro-bind and --dev-bind) and the --chdir directory.
func boundPaths(args []string) (binds []string, chdir string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bind", "--ro-bind", "--dev-bind", "--bind-try", "--ro-bind-try":
			if i+2 < len(args) {
				binds = append(binds, args[i+1], args[i+2])
				i += 2
			}
		case "--chdir":
			if i+1 < len(args) {
				chdir = args[i+1]
				i++
			}
		}
	}
	return binds, chdir
}

// Structural: the networked fetch binds no part of the workspace (it is
// covered by an empty tmpfs), runs in the fetch directory and writes only
// there and in the shared caches, both outside the workspace and the sandbox
// HOME.
func TestFetchSandboxSeesNoWorkspace_GEC1021R7(t *testing.T) {
	fakeWorkingBwrap(t)
	project := newNeedProject(t, "Cargo.toml", "[package]\nname = \"x\"\nversion = \"0.1.0\"\n", "src/main.rs", "fn main() {}\n",
		".cargo/config.toml", "[alias]\nfetch = \"build\"\n", ".git/config", "[remote \"o\"]\n\turl = https://x:token@h/r.git\n", ".env", "SECRET=1\n")
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"build"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fetch == nil || fetch.stage == nil {
		t.Fatalf("fetch = %+v, want a networked stage with a fetch directory", fetch)
	}
	dir := fetch.stage.dir.path
	if overlapping(dir, project.Path) || overlapping(fetch.stage.area.cache, project.Path) {
		t.Fatalf("fetch directory %s or cache %s overlaps the project %s", dir, fetch.stage.area.cache, project.Path)
	}
	spec := fetchSpec{workspace: project.Path, dir: dir, cache: fetch.stage.area.cache, pathEnv: fetchPathEnv(project.Path), env: fetch.env}
	_, args, err := wrapFetchCommand("/usr/bin/bwrap", spec, fetchToolPath("cargo", project.Path), fetch.Args)
	if err != nil {
		t.Fatal(err)
	}
	binds, chdir := boundPaths(args)
	for _, path := range binds {
		if pathWithin(project.Path, path) {
			t.Fatalf("the fetch binds %s, inside the workspace: %q", path, args)
		}
	}
	if chdir != dir {
		t.Fatalf("the fetch runs in %q, want its fetch directory %q", chdir, dir)
	}
	if i := slices.Index(args, project.Path); i < 1 || args[i-1] != "--tmpfs" {
		t.Fatalf("the workspace path is not covered by an empty tmpfs: %q", args)
	}
	// Only the manifests and empty target stubs are in the fetch directory.
	var copied []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			copied = append(copied, rel)
		}
		return nil
	})
	slices.Sort(copied)
	if want := []string{"Cargo.toml", filepath.Join("src", "main.rs")}; !slices.Equal(copied, want) {
		t.Fatalf("fetch directory holds %v, want %v", copied, want)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "src", "main.rs")); len(data) != 0 {
		t.Fatalf("src/main.rs stub holds %q, want an empty file", data)
	}
}

// Finding 1 (blocker), live with the real cargo, bubblewrap and slirp4netns: a
// workspace .cargo that is a symlink to a directory whose config.toml names a
// registry credential provider is never read by the networked cargo fetch. The
// alias it also sets makes cargo say when it reads the file. Fails on
// adfdf17, whose masks skipped a .cargo symlink.
func TestCargoFetchReadsNoSymlinkedWorkspaceConfig_GEC1021R7(t *testing.T) {
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("no cargo on PATH")
	}
	rustupHome := os.Getenv("RUSTUP_HOME")
	if userHome, err := os.UserHomeDir(); rustupHome == "" && err == nil {
		rustupHome = filepath.Join(userHome, ".rustup")
	}
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	unsetProxyEnv(t)
	t.Setenv("PATH", filepath.Dir(cargo)+":/usr/bin:/bin")
	t.Setenv("RUSTUP_HOME", rustupHome)
	t.Setenv("RUSTUP_TOOLCHAIN", "")
	liveSandbox(t)
	writeProjectFiles(t, project.Path,
		"Cargo.toml", "[package]\nname = \"x\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"src/main.rs", "fn main() {}\n",
		"cfg/config.toml", "[alias]\nfetch = \"build\"\n\n[registries.evil]\nindex = \"sparse+https://example.invalid/\"\ncredential-provider = [\""+filepath.Join(project.Path, "provider.sh")+"\"]\n")
	if err := os.Symlink("cfg", filepath.Join(project.Path, ".cargo")); err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Detector: "Cargo.toml", Command: "cargo", Args: []string{"fetch"}})
	out := ""
	if result != nil {
		out = result.Stdout + result.Stderr
	}
	if strings.Contains(out, "user-defined alias") {
		t.Fatalf("the networked cargo fetch read the symlinked workspace .cargo/config.toml (err %v):\n%s", err, out)
	}
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v, want the fetch to succeed", result, err)
	}
}

// Finding 2 (blocker), live: a loopback step (running project code) cannot
// plant anything in the module cache a later networked go fetch uses - such
// as a git credential helper in a cached repository's config under cache/vcs
// - because that cache lives outside the workspace and is read-only in every
// loopback step; nor does the fetch see the sandbox HOME's own cache. Fails on
// adfdf17, whose fetch reused the sandbox HOME's module cache.
func TestLoopbackStepCannotPlantInFetchModuleCache_GEC1021R7(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	tools := outsideTools(t)
	liveSandbox(t)
	base, err := depFetchBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	fetchCache := filepath.Join(base, "cache", fetchCacheGo)
	writeProjectFiles(t, project.Path, "go.mod", "module example.com/x\n\ngo 1.21\n")
	const plant = `import os, sys
helper = "[credential]\n\thelper = \"!touch /tmp/pwned\"\n"
planted = []
for cache in (os.environ.get("GOMODCACHE") or os.path.join(os.environ["HOME"], "go", "pkg", "mod"), sys.argv[1]):
    path = os.path.join(cache, "cache", "vcs", "deadbeef")
    try:
        os.makedirs(path, exist_ok=True)
        with open(os.path.join(path, "config"), "w") as f:
            f.write(helper)
        planted.append(cache)
    except OSError:
        pass
print("planted in", planted)
`
	if result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "tests", Command: "python3", Args: []string{"-c", plant, fetchCache}}); err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("loopback step: %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(fetchCache, "cache", "vcs", "deadbeef", "config")); err == nil {
		t.Fatal("a loopback step wrote into the fetch's module cache")
	}
	// The networked fetch reports any planted repository configuration in the
	// module cache it uses.
	writeExecScript(t, filepath.Join(tools, "go"), `[ "$1" = env ] && { echo '{}'; exit 0; }
if [ "$1" = mod ]; then
  if /bin/grep -rqs helper "${GOMODCACHE:-$HOME/go/pkg/mod}/cache/vcs" ; then
    echo "PLANTED: a loopback step's credential helper is in the fetch's module cache ${GOMODCACHE}" >&2
    exit 1
  fi
  echo "GOMODCACHE=$GOMODCACHE"
fi
exit 0
`)
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "download"}})
	if result != nil && strings.Contains(result.Stderr, "PLANTED") {
		t.Fatalf("the networked go fetch reuses a module cache a loopback step wrote:\n%s", result.Stderr)
	}
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if !strings.Contains(result.Stdout, "GOMODCACHE="+fetchCache) {
		t.Fatalf("the fetch's GOMODCACHE is not the shared cache %s:\n%s", fetchCache, result.Stdout)
	}
}

// Finding 3 (blocker), live: an executable in the workspace (a .venv/bin on
// PATH, ahead of the real tool) never runs as the networked fetch command: the
// fetch runs the trusted pip outside the workspace, and its PATH holds no
// workspace directory. Fails on adfdf17, which ran the first pip on the PATH.
func TestWorkspaceExecutableNeverRunsTheFetch_GEC1021R7(t *testing.T) {
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	tools := outsideTools(t)
	venv := filepath.Join(project.Path, ".venv", "bin")
	t.Setenv("PATH", venv+":"+tools+":/usr/bin:/bin")
	liveSandbox(t)
	writeExecScript(t, filepath.Join(venv, "pip"), `echo "WORKSPACE-PIP ran: $*"
exit 1
`)
	writeExecScript(t, filepath.Join(tools, "pip"), `case "$1" in
download) echo "TRUSTED-PIP download PATH=$PATH"; exit 1 ;;
esac
exit 0
`)
	writeProjectFiles(t, project.Path, "requirements.txt", "six==1.16.0\n")
	result, _ := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}})
	if result == nil {
		t.Fatal("no result")
	}
	if strings.Contains(result.Stdout, "WORKSPACE-PIP") {
		t.Fatalf("the workspace's .venv/bin/pip ran as the networked fetch:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "TRUSTED-PIP download") {
		t.Fatalf("the trusted pip did not run the download:\n%s%s", result.Stdout, result.Stderr)
	}
	if strings.Contains(result.Stdout, project.Path) {
		t.Fatalf("the fetch PATH holds a workspace directory:\n%s", result.Stdout)
	}
}

// Finding 3, unit: the fetch PATH drops every directory in the workspace,
// reached through a symlink too, and a tool whose #! interpreter lies in the
// workspace is not trusted.
func TestFetchToolPathSkipsTheWorkspace_GEC1021R7(t *testing.T) {
	swapVerifyIsolationVars(t)
	ws := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(outside, "link-bin")
	if err := os.Symlink(filepath.Join(ws, "bin"), link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(ws, ".venv", "bin"), filepath.Join(ws, "bin")} {
		writeExecScript(t, filepath.Join(dir, "pip"), "exit 0\n")
	}
	shebang := filepath.Join(outside, "shebang")
	writeExecScript(t, filepath.Join(shebang, "pip"), "")
	if err := os.WriteFile(filepath.Join(shebang, "pip"), []byte("#!"+filepath.Join(ws, ".venv", "bin", "python")+"\n"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	good := filepath.Join(outside, "good")
	writeExecScript(t, filepath.Join(good, "pip"), "exit 0\n")
	verifyGetenv = func(key string) string {
		if key == "PATH" {
			return strings.Join([]string{filepath.Join(ws, ".venv", "bin"), link, "relative/bin", shebang, good}, ":")
		}
		return ""
	}
	if got, want := fetchPathEnv(ws), shebang+":"+good; got != want {
		t.Fatalf("fetchPathEnv = %q, want %q", got, want)
	}
	if got, want := fetchToolPath("pip", ws), filepath.Join(good, "pip"); got != want {
		t.Fatalf("fetchToolPath = %q, want %q (skipping the one whose interpreter is in the workspace)", got, want)
	}
	if got := fetchToolPath("no-such-tool", ws); got != "no-such-tool" {
		t.Fatalf("fetchToolPath of a missing tool = %q, want the bare name", got)
	}
}

// Finding 4 (blocker): npm runs with the network only as `npm ci` or `npm
// install` without package arguments; `npm install ./dep` (whose prepare
// script npm before 12 runs while installing) and any other package argument
// is refused before anything runs. Fails on adfdf17, which checked the root
// manifest and then installed the command line's target.
func TestNpmPackageArgumentsNeverFetchWithNetwork_GEC1021R7(t *testing.T) {
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	for _, args := range [][]string{
		{"install", "./dep"},
		{"install", "file:dep"},
		{"i", "dep.tgz"},
		{"install", "github:user/repo"},
		{"install", "left-pad"},
		{"install", "--prefix", "sub"},
		{"ci", "--userconfig=evil.npmrc"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			env := installFakeUserNet(t, fakeUserNetOptions{})
			project, err := NewProject("npm-args", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(project.Path, "package.json"), `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"^1.3.0"}}`)
			writeFile(t, filepath.Join(project.Path, "package-lock.json"), registryLeftPadLock)
			writeFile(t, filepath.Join(project.Path, "dep", "package.json"), `{"name":"dep","version":"1.0.0","scripts":{"prepare":"touch PREPARED"}}`)
			result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: args})
			if !errorIsDepNetworkUnavailable(err) || !strings.Contains(noteOf(result), "without package arguments") {
				t.Fatalf("err = %v, note = %q, want the networked fetch refused for its package argument", err, noteOf(result))
			}
			if data := readLog(t, env.log); strings.Contains(data, "ran") {
				t.Fatalf("no sandbox may run when the fetch is refused; bwrap log:\n%s", data)
			}
		})
	}
	for _, args := range [][]string{{"install"}, {"ci"}, {"install", "--ignore-scripts", "--no-audit", "--omit=dev"}, {"ci", "--legacy-peer-deps"}} {
		if !slices.ContainsFunc(args[1:], func(a string) bool { return !npmNetworkedOption(a) }) {
			continue
		}
		t.Errorf("npm %v: an option a networked fetch should accept was refused", args)
	}
}

// Finding 4, live with the npm before 12 at /usr/bin (npm 10.8.2 on this
// host): `npm install ./dep` never runs the local dependency's prepare script,
// which that npm runs while installing it even under --ignore-scripts. Fails
// on adfdf17, which ran the install with the network in the workspace.
func TestNpmLocalPrepareNeverRunsLive_GEC1021R7(t *testing.T) {
	const npmBin = "/usr/bin/npm"
	if _, err := os.Stat(npmBin); err != nil {
		t.Skip("no npm at /usr/bin")
	}
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	unsetProxyEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	liveSandbox(t)
	marker := filepath.Join(project.Path, "dep", "PREPARED")
	// A registry-only (empty) lockfile, so nothing but the command line's
	// target could make npm prepare a directory.
	writeProjectFiles(t, project.Path,
		"package.json", `{"name":"x","version":"1.0.0"}`,
		"package-lock.json", `{"name":"x","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"x","version":"1.0.0"}}}`,
		"dep/package.json", `{"name":"dep","version":"1.0.0","scripts":{"prepare":"echo ran > PREPARED"}}`)
	plan := ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts", "./dep"},
		env: []string{"npm_config_audit=false", "npm_config_fund=false", "npm_config_update_notifier=false"}}
	_, err := project.RunVerificationPlan(context.Background(), plan)
	if data, _ := os.ReadFile(marker); len(data) > 0 {
		t.Fatalf("the local dependency's prepare ran during the networked install (err %v)", err)
	}
	if !errorIsDepNetworkUnavailable(err) {
		t.Fatalf("err = %v, want the networked fetch refused", err)
	}
}

// Finding 5 (major), with the swap simulated: while a concurrent workspace
// writer keeps exchanging .makewand for a symlink to a directory outside the
// workspace, the host's sandbox HOME writes (the go telemetry mode file) never
// land outside: they go through the os.Root handles the components were
// checked through, never a path reopened afterwards. Fails on adfdf17, which
// checked the components and then reopened the sandbox HOME by its absolute
// path.
func TestSandboxHomeWritesFollowNoSwappedPath_GEC1021R7(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "mw", "sandbox-home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".makewand", "sandbox-home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "mw"), filepath.Join(ws, ".makewand-swap")); err != nil {
		t.Fatal(err)
	}
	escaped := filepath.Join(outside, "mw", "sandbox-home", ".config", "go", "telemetry", "mode")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = unix.Renameat2(unix.AT_FDCWD, filepath.Join(ws, ".makewand"), unix.AT_FDCWD, filepath.Join(ws, ".makewand-swap"), unix.RENAME_EXCHANGE)
		}
	}()
	p := &Project{Path: ws}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = p.prepareSandboxHome(ws) // a refusal is expected while the swap is in place
		if _, err := os.Stat(escaped); err == nil {
			break
		}
	}
	close(stop)
	<-done
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("a host-side sandbox HOME write followed the swapped .makewand out of the workspace: %s exists", escaped)
	}
}

// Finding 5: the fetch directory is created and written through the handle of
// the fetch area, outside the workspace; a write never follows a symlink out
// of it, and an area that overlaps the workspace is refused.
func TestFetchDirWritesThroughItsHandle_GEC1021R7(t *testing.T) {
	ws := t.TempDir()
	area, err := openDepFetchArea(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer area.close()
	dir, err := area.newFetchDir()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.remove()
	if overlapping(dir.path, ws) {
		t.Fatalf("fetch directory %s overlaps the workspace", dir.path)
	}
	if info, err := os.Stat(dir.path); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("fetch directory mode = %v (%v), want 0700", info.Mode().Perm(), err)
	}
	victim := filepath.Join(t.TempDir(), "victim.txt")
	writeFile(t, victim, "original")
	if err := os.Symlink(victim, filepath.Join(dir.path, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := dir.writeFile("evil", []byte("overwritten"), 0o600); err == nil {
		t.Fatal("writeFile wrote through a symlink; want a refusal")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir.path, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := dir.writeFile(filepath.Join("sub", "x"), []byte("x"), 0o600); err == nil {
		t.Fatal("writeFile followed a symlinked directory; want a refusal")
	}
	if data, _ := os.ReadFile(victim); string(data) != "original" {
		t.Fatalf("victim overwritten: %q", data)
	}

	old := depFetchBaseDir
	depFetchBaseDir = func() (string, error) { return filepath.Join(ws, ".cache", "dep-fetch"), nil }
	defer func() { depFetchBaseDir = old }()
	if _, err := openDepFetchArea(ws); err == nil {
		t.Fatal("a fetch area inside the workspace was accepted")
	}
}

// Finding 6 (major), live with the real pip, bubblewrap, slirp4netns and
// PyPI: a pinned version that does not exist (requests==999999) is the
// candidate's own broken dependency, not an environment verdict, although
// the project exists. Fails on adfdf17, which only asked whether the project
// exists.
func TestNonexistentVersionIsCandidateFailureLive_GEC1021R7(t *testing.T) {
	pip, err := exec.LookPath("pip")
	if err != nil {
		t.Skip("no pip on PATH")
	}
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	unsetProxyEnv(t)
	t.Setenv("PATH", filepath.Dir(pip)+":/usr/bin:/bin")
	liveSandbox(t)
	if !pypiReachable() {
		t.Skip("pypi.org is not reachable from this host")
	}
	writeProjectFiles(t, project.Path, "requirements.txt", "requests==999999\n")
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}})
	if errorIsDepNetworkUnavailable(err) {
		t.Fatalf("a version that does not exist earned the environment verdict: %v", err)
	}
	if err != nil || result == nil || result.ExitCode == 0 {
		t.Fatalf("result = %+v, err = %v, want the failed fetch as the candidate's failure", result, err)
	}
	if !strings.Contains(result.Note, "no release of requests") {
		t.Fatalf("note = %q, want it to say no release matches", result.Note)
	}
}

// pypiReachable reports whether the host reaches PyPI directly.
func pypiReachable() bool {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Head("https://pypi.org/simple/requests/")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Finding 6, live: a URL dependency whose environment marker does not hold
// (python_version < "2") is no unfetched remote target, so a build that fails
// for its own reason stays the candidate's failure. Fails on adfdf17, which
// did not evaluate the marker.
func TestFalseMarkerRemoteDependencyStaysCandidate_GEC1021R7(t *testing.T) {
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	tools := outsideTools(t)
	liveSandbox(t)
	writeExecScript(t, filepath.Join(tools, "pip"), `case "$1" in
download) exit 0 ;;
esac
echo "error: the build backend failed for its own reason" >&2
exit 1
`)
	writeProjectFiles(t, project.Path, "pyproject.toml",
		"[project]\nname = \"x\"\nversion = \"0\"\ndependencies = [\"six\", 'bait @ https://example.invalid/x.whl ; python_version < \"2\"']\n")
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-e", "."}})
	if errorIsDepNetworkUnavailable(err) {
		t.Fatalf("a URL dependency whose marker does not hold earned the environment verdict: %v", err)
	}
	if err != nil || result == nil || result.ExitCode == 0 {
		t.Fatalf("result = %+v, err = %v, want the failing build as the candidate's failure", result, err)
	}
}

// Finding 6, unit: the remote target is counted only when its marker holds
// for the interpreter, and not when makewand cannot evaluate it.
func TestPipUnfetchedRemoteTargetEvaluatesMarkers_GEC1021R7(t *testing.T) {
	old := pipMarkerEnvironment
	pipMarkerEnvironment = func(string) pipMarkerEnv {
		return pipMarkerEnv{"python_version": "3.12", "python_full_version": "3.12.3", "sys_platform": "linux", "implementation_name": "cpython", "platform_machine": "x86_64", "os_name": "posix"}
	}
	t.Cleanup(func() { pipMarkerEnvironment = old })
	for _, tc := range []struct {
		req  string
		want bool
	}{
		{`bait @ https://h.example/x.whl ; python_version < "2"`, false},
		{`bait @ https://h.example/x.whl ; sys_platform == "win32"`, false},
		{`bait @ https://h.example/x.whl ; python_version >= "3.8" and sys_platform == "linux"`, true},
		{`git+https://h.example/r.git#egg=r ; os_name == "posix"`, true},
		{`bait @ https://h.example/x.whl ; platform_release >= "5"`, false}, // not a version: cannot be evaluated
		{`bait @ https://h.example/x.whl`, true},
	} {
		project := newNeedProject(t, "requirements.txt", tc.req+"\n")
		got := project.pipUnfetchedRemoteTarget(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-r", "requirements.txt"}})
		if (got != "") != tc.want {
			t.Errorf("%s: pipUnfetchedRemoteTarget = %q, want counted: %v", tc.req, got, tc.want)
		}
	}
}

// Finding 7 (major): the package sources pip configuration names - the
// project's pip.conf or pip.ini, the sandbox HOME's pip.conf - reach the
// networked fetch: http(s) indexes and trusted hosts as options, a local
// find-links directory of wheels as a copy in the fetch directory. A source
// the fetch cannot be given refuses it instead of falling back to PyPI. Fails
// on adfdf17, which read no configuration and dropped a local find-links
// directory, so the fetch asked PyPI.
func TestPipPrivateSourcesNeverReplacedByPyPI_GEC1021R7(t *testing.T) {
	plan := ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-r", "requirements.txt"}}
	t.Run("the project's pip.conf", func(t *testing.T) {
		project := newNeedProject(t, "requirements.txt", "internal-lib==1.0\n",
			"pip.conf", "[global]\nindex-url = https://pypi.corp.example/simple\nextra-index-url =\n    https://extra.corp.example/simple\ntrusted-host = pypi.corp.example\n")
		fetch, _, cleanup, err := project.planDependencyStages(plan, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		joined := strings.Join(fetch.Args, " ")
		for _, want := range []string{"--index-url https://pypi.corp.example/simple", "--extra-index-url https://extra.corp.example/simple", "--trusted-host pypi.corp.example"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("fetch args %q, want %q", joined, want)
			}
		}
		if got := fetch.stage.pip.indexes(); !slices.Equal(got, []string{"https://pypi.corp.example/simple", "https://extra.corp.example/simple"}) {
			t.Fatalf("indexes the classification asks = %v", got)
		}
	})
	t.Run("the sandbox HOME's pip.conf, overridden by the command line", func(t *testing.T) {
		project := newNeedProject(t, "requirements.txt", "internal-lib==1.0\n",
			filepath.Join(sandboxHomeRelPath, ".config", "pip", "pip.conf"), "[install]\nindex-url = https://home.corp.example/simple\n")
		fetch, _, cleanup, err := project.planDependencyStages(plan, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if !strings.Contains(strings.Join(fetch.Args, " "), "--index-url https://home.corp.example/simple") {
			t.Fatalf("fetch args %q, want the sandbox HOME's index", fetch.Args)
		}
		withCLI := plan
		withCLI.Args = append(slices.Clone(plan.Args), "-i", "https://cli.example/simple")
		fetch, _, cleanup2, err := project.planDependencyStages(withCLI, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup2()
		if joined := strings.Join(fetch.Args, " "); !strings.Contains(joined, "--index-url https://cli.example/simple") || strings.Contains(joined, "home.corp.example") {
			t.Fatalf("fetch args %q, want the command line's index only", joined)
		}
	})
	t.Run("--no-index --find-links ./wheels", func(t *testing.T) {
		project := newNeedProject(t, "requirements.txt", "--no-index\n--find-links ./wheels\nlocalpkg==1.0\n",
			"wheels/localpkg-1.0-py3-none-any.whl", "wheel bytes", "wheels/notes.txt", "not a wheel")
		fetch, _, cleanup, err := project.planDependencyStages(plan, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		fl := slices.Index(fetch.Args, "--find-links")
		if !slices.Contains(fetch.Args, "--no-index") || fl < 0 || fetch.Args[fl+1] != fetch.stage.dir.abs("wheels") {
			t.Fatalf("fetch args %q, want --no-index and the copied wheels directory", fetch.Args)
		}
		if data, err := os.ReadFile(filepath.Join(fetch.stage.dir.abs("wheels"), "localpkg-1.0-py3-none-any.whl")); err != nil || string(data) != "wheel bytes" {
			t.Fatalf("the wheel was not copied: %q, %v", data, err)
		}
		if _, err := os.Stat(filepath.Join(fetch.stage.dir.abs("wheels"), "notes.txt")); err == nil {
			t.Fatal("a file that is no wheel was copied")
		}
	})
	for _, tc := range []struct {
		name, file, content string
		args                []string
	}{
		{"a file: index in pip.conf", "pip.conf", "[global]\nindex-url = file:///srv/pypi\n", nil},
		{"a local extra index on the command line", "", "", []string{"--extra-index-url", "/srv/pypi"}},
		{"a find-links directory outside the project", "", "", []string{"--find-links", "/srv/wheels"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []string{"requirements.txt", "internal-lib==1.0\n"}
			if tc.file != "" {
				files = append(files, tc.file, tc.content)
			}
			project := newNeedProject(t, files...)
			p := plan
			p.Args = append(slices.Clone(plan.Args), tc.args...)
			_, _, _, err := project.planDependencyStages(p, project.Path)
			var refusal *fetchRefusal
			if !errors.As(err, &refusal) || !strings.Contains(refusal.reason, "never replaces a configured package source with PyPI") {
				t.Fatalf("err = %v, want the networked fetch refused", err)
			}
		})
	}
}

// Finding 8 (minor): a constraints file's nested -c (and -r, which pip reads
// as requirements there) reaches the fetch, so it downloads the version the
// offline install requires. Fails on adfdf17, which skipped every option line
// of a constraints file.
func TestNestedPipConstraintsReachFetch_GEC1021R7(t *testing.T) {
	project := newNeedProject(t,
		"requirements.txt", "urllib3>=1\n",
		"constraints.txt", "-c more.txt\n-r extra-reqs.txt\n",
		"more.txt", "urllib3==1.26.20\n-c deeper/pins.txt\n",
		"deeper/pins.txt", "idna==3.7\n",
		"extra-reqs.txt", "certifi\n")
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-r", "requirements.txt", "-c", "constraints.txt"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ci := slices.Index(fetch.Args, "--constraint")
	if ci < 0 {
		t.Fatalf("fetch args %q, want a constraints file", fetch.Args)
	}
	data, err := os.ReadFile(fetch.Args[ci+1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"urllib3==1.26.20", "idna==3.7"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("constraints = %q, want the nested pin %s", data, want)
		}
	}
	if !slices.Contains(fetch.Args, "certifi") {
		t.Fatalf("fetch args %q, want the requirement a constraints file's -r names", fetch.Args)
	}
}
