//go:build linux

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/trustedbin"
)

// fakeArgsRecordingBwrap emulates a sandbox that runs every command
// successfully and records, for a pip step, its arguments and the distutils
// configuration in the sandbox HOME.
func fakeArgsRecordingBwrap(logPath string) string {
	return fmt.Sprintf(`log=%q
home=
while [ $# -gt 0 ]; do
  case "$1" in
    --setenv) [ "$2" = HOME ] && home=$3; shift 3 ;;
    --chdir|--tmpfs|--proc|--dev|--seccomp) shift 2 ;;
    --bind|--ro-bind) shift 3 ;;
    --*) shift ;;
    *) break ;;
  esac
done
if [ "$1" = pip ]; then
  config=none
  [ -f "$home/.pydistutils.cfg" ] && config=$(/usr/bin/tr '\n' ';' < "$home/.pydistutils.cfg")
  echo "$* | config: $config" >> "$log"
fi
[ "$1" = pytest ] && echo "1 passed"
exit 0
`, logPath)
}

// A sandboxed install of a setup.py-only project (an editable "-e ."
// requirement) goes through PEP 517 with egg_base redirected to the sandbox
// /tmp, like a pyproject.toml project: the legacy `setup.py develop` would
// write <name>.egg-info into the workspace and cannot be redirected. A project
// that is neither gets neither.
func TestSetupPyOnlyInstallRedirectsEggInfo_R3(t *testing.T) {
	setupPy := ExtractedFile{Path: "setup.py", Content: "from setuptools import setup\nsetup(name=\"fvlegacy\", version=\"0.1.0\", packages=[\"fvlegacy\"])\n"}
	editable := ExtractedFile{Path: "requirements.txt", Content: "-e .\n"}
	pyproject := ExtractedFile{Path: "pyproject.toml", Content: "[project]\nname = \"fvlegacy\"\nversion = \"0.1.0\"\n"}
	redirected := "config: [egg_info];egg_base = /tmp;"
	cases := []struct {
		name     string
		manifest []ExtractedFile
		pep517   bool
		config   string
	}{
		{"setup.py only", []ExtractedFile{setupPy, editable}, true, redirected},
		{"setup.py and pyproject.toml", []ExtractedFile{setupPy, editable, pyproject}, true, redirected},
		{"requirements only", []ExtractedFile{{Path: "requirements.txt", Content: "six==1.16.0\n"}}, false, "config: none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeWorkingBwrap(t)
			swapSeccompVars(t, "linux", false, nil)
			oldLookPath := userNetLookPath
			userNetLookPath = func(string) (string, error) { return "", fmt.Errorf("%w: slirp4netns", trustedbin.ErrNotFound) }
			t.Cleanup(func() { userNetLookPath = oldLookPath })
			t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
			dir := t.TempDir()
			log := filepath.Join(dir, "bwrap.log")
			bwrap := filepath.Join(dir, "bwrap")
			writeExecScript(t, bwrap, fakeArgsRecordingBwrap(log))
			verifyLookPath = func(string) (string, error) { return bwrap, nil }
			project, err := NewProject("egg-info-r3", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			files := append([]ExtractedFile{{Path: "fvlegacy/__init__.py", Content: "def f():\n    return 1\n"}, {Path: "test_f.py", Content: "from fvlegacy import f\n\ndef test_f():\n    assert f() == 1\n"}, {Path: "pytest.ini", Content: "[pytest]\n"}}, tc.manifest...)
			if err := project.WriteFiles(files); err != nil {
				t.Fatal(err)
			}
			report := project.VerifyRestrictedWorkspace(context.Background(), files[:1])
			if report.DepsError != "" || report.IntegrityError != "" {
				t.Fatalf("DepsError=%q IntegrityError=%q", report.DepsError, report.IntegrityError)
			}
			// The install is split into a networked wheel download and an
			// offline install from the wheelhouse; both run with the egg-info
			// redirect in the sandbox HOME, and only the install builds.
			lines := strings.Split(strings.TrimSpace(readLog(t, log)), "\n")
			var fetch, install string
			for _, line := range lines {
				cmd, _, _ := strings.Cut(line, " | ")
				switch {
				case strings.HasPrefix(cmd, "pip download "):
					fetch = line
				case strings.HasPrefix(cmd, "pip install "):
					install = line
				}
			}
			if fetch == "" || install == "" {
				t.Fatalf("pip steps:\n%s\nwant a wheel download and an install", strings.Join(lines, "\n"))
			}
			if !strings.Contains(fetch, "--only-binary=:all:") || !strings.HasSuffix(fetch, "| "+tc.config) {
				t.Fatalf("pip download step %q, want wheels only and %q", fetch, tc.config)
			}
			installCmd, _, _ := strings.Cut(install, " | ")
			if !strings.Contains(installCmd, "--no-index") || !strings.Contains(installCmd, "--find-links") || !strings.HasSuffix(install, "| "+tc.config) {
				t.Fatalf("pip install step %q, want an offline install and %q", install, tc.config)
			}
			if tc.pep517 != strings.Contains(installCmd, "--use-pep517") {
				t.Fatalf("pip install step %q, --use-pep517 want %v", installCmd, tc.pep517)
			}
			if report.DepsPlan == nil || slicesContainString(report.DepsPlan.Args, "--use-pep517") {
				t.Fatalf("reported DepsPlan = %+v, want the baseline plan", report.DepsPlan)
			}
			if _, err := os.Lstat(filepath.Join(project.Path, filepath.FromSlash(sandboxHomeRelPath), ".pydistutils.cfg")); !os.IsNotExist(err) {
				t.Fatalf("distutils configuration left behind: %v", err)
			}
		})
	}
}

// fakeHostPip records its arguments and the distutils configuration that
// DIST_EXTRA_CONFIG names, and checks that the egg_base it names is outside
// the workspace it runs in.
const fakeHostPip = `echo "args: $*" >> "$FAKE_PIP_LOG"
if [ -n "$DIST_EXTRA_CONFIG" ]; then
  echo "config: $(/usr/bin/tr '\n' ';' < "$DIST_EXTRA_CONFIG")" >> "$FAKE_PIP_LOG"
  echo "config-dir: $(/usr/bin/dirname "$DIST_EXTRA_CONFIG")" >> "$FAKE_PIP_LOG"
else
  echo "config: none" >> "$FAKE_PIP_LOG"
fi
exit 0
`

// Under the acknowledged unsafe opt-in the install keeps the user's HOME, so
// the redirect comes through DIST_EXTRA_CONFIG instead: a configuration in a
// fresh directory outside the workspace, which also receives the egg-info and
// is removed afterwards.
func TestHostInstallRedirectsEggInfo_R3(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest ExtractedFile
		args     string
	}{
		{"setup.py only", ExtractedFile{Path: "setup.py", Content: "from setuptools import setup\nsetup(name=\"fvlegacy\", version=\"0.1.0\", packages=[\"fvlegacy\"])\n"}, "args: install --user -r requirements.txt --use-pep517"},
		{"pyproject.toml", ExtractedFile{Path: "pyproject.toml", Content: "[project]\nname = \"fvlegacy\"\nversion = \"0.1.0\"\n"}, "args: install --user -r requirements.txt --use-pep517"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := newHostDepsProject(t, tc.manifest, ExtractedFile{Path: "requirements.txt", Content: "-e .\n"}, ExtractedFile{Path: "fvlegacy/__init__.py", Content: "X = 1\n"})
			tools := t.TempDir()
			writeExecScript(t, filepath.Join(tools, "pip"), fakeHostPip)
			t.Setenv("PATH", tools)
			log := filepath.Join(t.TempDir(), "pip.log")
			t.Setenv("FAKE_PIP_LOG", log)
			report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "fvlegacy/__init__.py", Content: "X = 2\n"}})
			if err != nil {
				t.Fatalf("EvaluateCandidateFiles: %v", err)
			}
			if report.DepsError != "" || report.IntegrityError != "" || report.Isolated {
				t.Fatalf("Isolated=%v DepsError=%q IntegrityError=%q", report.Isolated, report.DepsError, report.IntegrityError)
			}
			lines := strings.Split(strings.TrimSpace(readLog(t, log)), "\n")
			if len(lines) != 3 || lines[0] != tc.args {
				t.Fatalf("pip saw:\n%s\nwant %q and a redirect", strings.Join(lines, "\n"), tc.args)
			}
			configDir := strings.TrimPrefix(lines[2], "config-dir: ")
			if lines[1] != "config: [egg_info];egg_base = "+configDir+";" || !strings.Contains(configDir, "makewand-egg-base-") {
				t.Fatalf("pip saw %q in %q, want egg_base set to that private directory", lines[1], configDir)
			}
			if _, err := os.Stat(configDir); !os.IsNotExist(err) {
				t.Fatalf("egg_base directory left behind: %v", err)
			}
		})
	}
}

// Live: real setuptools, asked for build requirements the way pip asks a
// setup.py-only project under PEP 517, writes no egg-info into the
// verified workspace - in the sandbox and on the host.
func TestSetupPyOnlySetuptoolsWritesNoEggInfoIntoWorkspace_R3(t *testing.T) {
	if testing.Short() {
		t.Skip("live setuptools test")
	}
	if err := exec.Command("/usr/bin/python3", "-I", "-c", "import setuptools.build_meta as b; b.__legacy__.get_requires_for_build_editable").Run(); err != nil {
		t.Skipf("system python3 has no setuptools with PEP 660 support: %v", err)
	}
	files := []ExtractedFile{
		{Path: "setup.py", Content: "from setuptools import setup\nsetup(name=\"fvlegacy\", version=\"0.1.0\", packages=[\"fvlegacy\"])\n"},
		{Path: "requirements.txt", Content: "-e .\n"},
		{Path: "fvlegacy/__init__.py", Content: "def f():\n    return 1\n"},
		{Path: "test_f.py", Content: "from fvlegacy import f\n\ndef test_f():\n    assert f() == 1\n"},
		{Path: "pytest.ini", Content: "[pytest]\n"},
	}
	// The install is split into a wheel download (fetch-only) and an offline
	// install; the download runs no build, and the install must go through PEP
	// 517, so the fake pip passes `download` and refuses the legacy path (no
	// --use-pep517), which would need the egg-info in the source tree.
	const pip = "case \"$1\" in download) exit 0 ;; esac\n" +
		"case \" $* \" in *\" --use-pep517 \"*) ;; *) echo 'legacy setup.py develop' >&2; exit 1 ;; esac\n" +
		"exec /usr/bin/python3 -c 'import setuptools.build_meta as b; b.__legacy__.get_requires_for_build_editable()'\n"
	for _, mode := range []string{"sandbox", "host"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
			t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
			var project *Project
			if mode == "host" {
				// The host install reads the redirect from DIST_EXTRA_CONFIG (setuptools 65.2 and later).
				if err := exec.Command("/usr/bin/python3", "-I", "-c", "import inspect, setuptools._distutils.dist as d; assert 'DIST_EXTRA_CONFIG' in inspect.getsource(d)").Run(); err != nil {
					t.Skipf("system setuptools does not read DIST_EXTRA_CONFIG: %v", err)
				}
				project = newHostDepsProject(t)
			} else {
				t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
				var err error
				if project, err = NewProject("egg-info-r3-live", t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			tools := filepath.Join(t.TempDir(), "tools")
			if mode == "sandbox" {
				tools = filepath.Join(project.Path, "tools")
			}
			t.Setenv("PATH", tools+":/usr/bin:/bin")
			if mode == "sandbox" && !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
				t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
			}
			if err := project.WriteFiles(files); err != nil {
				t.Fatal(err)
			}
			writeExecScript(t, filepath.Join(tools, "pip"), pip)
			writeExecScript(t, filepath.Join(tools, "pytest"), "echo '1 passed'\n")
			report := project.VerifyRestrictedWorkspace(context.Background(), files[2:3])
			if report.IntegrityError != "" || report.DepsError != "" || report.Isolated != (mode == "sandbox") {
				t.Fatalf("Isolated=%v IntegrityError=%q DepsError=%q", report.Isolated, report.IntegrityError, report.DepsError)
			}
			if _, err := os.Stat(filepath.Join(project.Path, "fvlegacy.egg-info")); !os.IsNotExist(err) {
				t.Fatalf("egg-info written into the workspace: %v", err)
			}
		})
	}
}

// fakeLockingCargo stands in for cargo: like the real one it writes
// Cargo.lock when there is none and rewrites it when Cargo.toml names a crate
// it lacks - unless --locked forbids that.
const fakeLockingCargo = `echo "$*" >> "$FAKE_CARGO_LOG"
locked=0
for a in "$@"; do [ "$a" = --locked ] && locked=1; done
stale=0
for crate in $(/usr/bin/sed -n 's/^\([a-z][a-z0-9_-]*\) *= *".*/\1/p' Cargo.toml); do
  case "$crate" in name|version|edition) continue ;; esac
  [ -f Cargo.lock ] && /usr/bin/grep -q "name = \"$crate\"" Cargo.lock || stale=1
done
if [ ! -f Cargo.lock ] || [ "$stale" = 1 ]; then
  if [ "$locked" = 1 ]; then
    printf '%s\n' "error: cannot update the lock file $PWD/Cargo.lock because --locked was passed to prevent this" "help: to generate the lock file without accessing the network, remove the --locked flag and use --offline instead." >&2
    exit 101
  fi
  { echo "version = 4"; for crate in $(/usr/bin/sed -n 's/^\([a-z][a-z0-9_-]*\) *= *".*/\1/p' Cargo.toml); do echo "name = \"$crate\""; done; } > Cargo.lock
fi
[ "$1" = test ] && printf '%s\n' "running 1 test" "test tests::renders ... ok" "test result: ok. 1 passed; 0 failed"
exit 0
`

func newLockingCargoProject(t *testing.T, files ...ExtractedFile) (*Project, string) {
	t.Helper()
	project := newHostDepsProject(t, append([]ExtractedFile{{Path: "src/main.rs", Content: "fn main() {}\n"}}, files...)...)
	dir := t.TempDir()
	writeExecScript(t, filepath.Join(dir, "cargo"), fakeLockingCargo)
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	log := filepath.Join(t.TempDir(), "cargo.log")
	t.Setenv("FAKE_CARGO_LOG", log)
	return project, log
}

const cargoManifestItoa = "[package]\nname = \"fv\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nitoa = \"1\"\n"

// A Cargo.lock that cargo generates for a project without one is cargo's
// doing, like npm's: verification removes it again instead of reporting a
// changed input.
func TestCargoLockfileGeneratedForProjectWithoutOne_R3(t *testing.T) {
	for _, manifest := range []string{cargoManifestItoa, "[package]\nname = \"fv\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\n"} {
		project, log := newLockingCargoProject(t, ExtractedFile{Path: "Cargo.toml", Content: manifest})
		report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "src/main.rs", Content: "fn main() {}\n// candidate\n"}})
		if err != nil {
			t.Fatalf("EvaluateCandidateFiles: %v", err)
		}
		if !report.Passed || report.IntegrityError != "" {
			t.Fatalf("Passed=%v IntegrityError=%q QuickCheckError=%q TestsError=%q, want a pass", report.Passed, report.IntegrityError, report.QuickCheckError, report.TestsError)
		}
		if got := readLog(t, log); strings.Contains(got, "--locked") {
			t.Fatalf("cargo ran --locked without a lockfile to honour:\n%s", got)
		}
	}
}

// A candidate that adds a dependency but leaves the committed Cargo.lock
// stale gets that reported, as its own failure, not a generic integrity
// error after cargo rewrote the lockfile.
func TestCargoStaleLockfileIsReportedClearly_R3(t *testing.T) {
	lock := ExtractedFile{Path: "Cargo.lock", Content: "version = 4\nname = \"itoa\"\n"}
	addRyu := ExtractedFile{Path: "Cargo.toml", Content: cargoManifestItoa + "ryu = \"1\"\n"}
	main := ExtractedFile{Path: "src/main.rs", Content: "fn main() {}\n// candidate\n"}

	project, _ := newLockingCargoProject(t, ExtractedFile{Path: "Cargo.toml", Content: cargoManifestItoa}, lock)
	files := []ExtractedFile{addRyu, main}
	report, err := project.EvaluateCandidateFiles(context.Background(), files)
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.Passed || report.EnvironmentError || report.IntegrityError != "" ||
		!strings.Contains(report.QuickCheckError, "dependencies changed but the lockfile was not updated: Cargo.lock does not match Cargo.toml") {
		t.Fatalf("Passed=%v EnvironmentError=%v IntegrityError=%q QuickCheckError=%q, want the stale lockfile reported", report.Passed, report.EnvironmentError, report.IntegrityError, report.QuickCheckError)
	}
	attempt := CandidateAttempt{Provider: "claude", Requested: "claude", Files: files, Content: RenderExtractedFiles(files), Verification: report}
	if stage := CandidateAttemptStage(context.Background(), attempt); stage != CandidateProgressRejected {
		t.Fatalf("stage = %v, want rejected", stage)
	}

	// Updating the lockfile with the manifest passes.
	project, _ = newLockingCargoProject(t, ExtractedFile{Path: "Cargo.toml", Content: cargoManifestItoa}, lock)
	report, err = project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{addRyu, {Path: "Cargo.lock", Content: lock.Content + "name = \"ryu\"\n"}, main})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed || report.IntegrityError != "" {
		t.Fatalf("Passed=%v IntegrityError=%q QuickCheckError=%q, want a pass", report.Passed, report.IntegrityError, report.QuickCheckError)
	}
}

// Live: a crate without dependencies and without a Cargo.lock verifies with
// the real cargo, which generates the lockfile (offline: nothing to fetch).
func TestCargoProjectWithoutLockfileVerifiesLive_R3(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo not installed")
	}
	if os.Getenv("RUSTUP_HOME") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			t.Setenv("RUSTUP_HOME", filepath.Join(home, ".rustup"))
		}
	}
	t.Setenv("PATH", filepath.Dir(cargo)+":/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "2G")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	ws := t.TempDir()
	main := "fn render(n: u64) -> String {\n    n.to_string()\n}\n\nfn main() {\n    println!(\"{}\", render(42));\n}\n\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn renders() {\n        assert_eq!(super::render(7), \"7\");\n    }\n}\n"
	writeFile(t, filepath.Join(ws, "Cargo.toml"), "[package]\nname = \"fvnodep\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\n")
	writeFile(t, filepath.Join(ws, "src", "main.rs"), main)
	project, err := OpenProject(ws)
	if err != nil {
		t.Fatal(err)
	}
	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "src/main.rs", Content: main + "// candidate\n"}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed || report.IntegrityError != "" {
		t.Fatalf("Passed=%v IntegrityError=%q QuickCheckError=%q TestsError=%q, want a pass", report.Passed, report.IntegrityError, report.QuickCheckError, report.TestsError)
	}
}

// A crate without dependencies runs its offline steps without a fetch.
func TestRustCrateWithoutDependenciesSkipsFetch_R3(t *testing.T) {
	project, log := newLockingCargoProject(t,
		ExtractedFile{Path: "Cargo.toml", Content: "[package]\nname = \"fv\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\n"},
		ExtractedFile{Path: "Cargo.lock", Content: "version = 4\n"},
	)
	report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "src/main.rs", Content: "fn main() {}\n// candidate\n"}})
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed {
		t.Fatalf("Passed=%v QuickCheckError=%q TestsError=%q", report.Passed, report.QuickCheckError, report.TestsError)
	}
	if got := strings.TrimSpace(readLog(t, log)); got != "check --tests --locked\ntest --locked" {
		t.Fatalf("cargo invocations = %q, want the quick check and tests without a fetch", got)
	}
}
