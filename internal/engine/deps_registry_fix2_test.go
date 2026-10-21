//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/trustedbin"
)

func writeExecScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// newHostDepsProject creates a project whose restricted plans run on the host
// (the acknowledged unsafe opt-in), so fake tools on PATH stand in for npm,
// pip, go and cargo.
func newHostDepsProject(t *testing.T, files ...ExtractedFile) *Project {
	t.Helper()
	allowHostExecForTest(t)
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	project, err := NewProject("deps-fix2", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	project.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{Acknowledged: true, Source: "test"})
	if len(files) > 0 {
		if err := project.WriteFiles(files); err != nil {
			t.Fatalf("WriteFiles: %v", err)
		}
		if err := project.ScanFiles(); err != nil {
			t.Fatalf("ScanFiles: %v", err)
		}
	}
	return project
}

// fakeToolPrinting makes name the only tool on PATH; it prints stderr and
// exits with exit.
func fakeToolPrinting(t *testing.T, name, stderr string, exit int) {
	t.Helper()
	dir := t.TempDir()
	writeExecScript(t, filepath.Join(dir, name), fmt.Sprintf("/bin/cat >&2 <<'MAKEWAND_EOF'\n%s\nMAKEWAND_EOF\nexit %d\n", stderr, exit))
	t.Setenv("PATH", dir)
}

// Real outputs of the package managers (npm 12, pip 24/26, go 1.27,
// cargo 1.85) and of rustc, captured offline.
const (
	npmRegistryENOTFOUND = `npm error code ENOTFOUND
npm error syscall getaddrinfo
npm error errno ENOTFOUND
npm error network request to https://registry.npmjs.org/left-pad failed, reason: getaddrinfo ENOTFOUND registry.npmjs.org
npm error network This is a problem related to network connectivity.
npm error network In most cases you are behind a proxy or have bad network settings.`

	pipIndexUnreachable = `WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NameResolutionError("HTTPSConnection(host='pypi.org', port=443): Failed to resolve 'pypi.org' ([Errno -2] Name or service not known)")': /simple/six/
ERROR: Could not find a version that satisfies the requirement six==1.16.0 (from versions: none)
ERROR: No matching distribution found for six==1.16.0`

	pipBuildDepsUnreachable = `  Installing build dependencies: started
  Installing build dependencies: finished with status 'error'
  error: subprocess-exited-with-error

  × installing build dependencies did not run successfully.
  │ exit code: 1
  ╰─> [3 lines of output]
      WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NameResolutionError("HTTPSConnection(host='pypi.org', port=443): Failed to resolve 'pypi.org' ([Errno -2] Name or service not known)")': /simple/setuptools/
      ERROR: Could not find a version that satisfies the requirement setuptools>=61 (from versions: none)
      ERROR: No matching distribution found for setuptools>=61
      [end of output]

  note: This error originates from a subprocess, and is likely not a problem with pip.
ERROR: Failed to build 'file:///tmp/ws' when installing build dependencies`

	pip24BuildDepsUnreachable = `  Installing build dependencies: started
  Installing build dependencies: finished with status 'error'
  error: subprocess-exited-with-error

  × pip subprocess to install build dependencies did not run successfully.
  │ exit code: 1
  ╰─> [3 lines of output]
      WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NewConnectionError('<pip._vendor.urllib3.connection.HTTPSConnection object at 0x7b26d2d8a810>: Failed to establish a new connection: [Errno -2] Name or service not known')': /simple/setuptools/
      ERROR: Could not find a version that satisfies the requirement setuptools>=61 (from versions: none)
      ERROR: No matching distribution found for setuptools>=61
      [end of output]

  note: This error originates from a subprocess, and is likely not a problem with pip.
error: subprocess-exited-with-error

× pip subprocess to install build dependencies did not run successfully.
│ exit code: 1
╰─> See above for output.`

	cargoIndexUnreachable = `    Updating crates.io index
warning: spurious network error (1 try remaining): [6] Could not resolve hostname (Could not resolve host: index.crates.io)
error: failed to get ` + "`itoa`" + ` as a dependency of package ` + "`fvdep v0.1.0 (/tmp/ws)`" + `

Caused by:
  download of config.json failed

Caused by:
  failed to download from ` + "`https://index.crates.io/config.json`" + `

Caused by:
  [6] Could not resolve hostname (Could not resolve host: index.crates.io)`

	cargoGitDependencyUnreachable = "    Updating git repository `https://nonexistent.invalid/foo`\n" +
		"warning: spurious network error (1 try remaining): failed to resolve address for nonexistent.invalid: Name or service not known; class=Net (12)\n" +
		"error: failed to get `foo` as a dependency of package `gd v0.1.0 (/tmp/ws)`\n\n" +
		"Caused by:\n  failed to load source for dependency `foo`\n\n" +
		"Caused by:\n  Unable to update https://nonexistent.invalid/foo\n\n" +
		"Caused by:\n  failed to clone into: /tmp/h/.cargo/git/db/foo-709285477ccdaf8f\n\n" +
		"Caused by:\n  network failure seems to have happened\n\n" +
		"Caused by:\n  failed to resolve address for nonexistent.invalid: Name or service not known; class=Net (12)"

	rustcQuotesNetworkWords = "   Compiling fv v0.1.0 (/tmp/ws)\n" +
		"error[E0308]: mismatched types\n" +
		" --> src/main.rs:2:24\n" +
		"  |\n" +
		"2 |     let retries: u32 = \"dial tcp failed: connection refused\";\n" +
		"  |                  ---   ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^ expected `u32`, found `&str`\n" +
		"  |                  |\n" +
		"  |                  expected due to this\n\n" +
		"For more information about this error, try `rustc --explain E0308`.\n" +
		"error: could not compile `fv` (bin \"fv\") due to 1 previous error"

	rustcCompileErrorMimicsCargo = "   Compiling fv v0.1.0 (/tmp/ws)\n" +
		"error: failed to download from `https://index.crates.io/config.json`\n" +
		" --> src/main.rs:1:1\n" +
		"  |\n" +
		"1 | compile_error!(\"failed to download from `https://index.crates.io/config.json`\");\n" +
		"  | ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^\n\n" +
		"error: could not compile `fv` (bin \"fv\") due to 1 previous error"
)

// A failed dependency install is an environment error only on the package
// manager's own evidence that its registry, index or module proxy could not
// be reached - never because compiler output, build backend output or a
// quoted server response happens to contain network phrases, and never for a
// URL the project's manifest names itself.
func TestDependencyRegistryOutageClassification_FixN1R2(t *testing.T) {
	urlDependency := ExtractedFile{Path: "package.json", Content: `{"name":"x","version":"1.0.0","dependencies":{"y":"https://nonexistent.invalid/y-1.0.0.tgz"}}`}
	cases := []struct {
		name     string
		command  string
		args     []string
		manifest []ExtractedFile
		stderr   string
		exit     int
		want     bool
	}{
		// Registry, index and proxy outages stay environment errors.
		{name: "npm registry ENOTFOUND", command: "npm", args: []string{"install"}, stderr: npmRegistryENOTFOUND, exit: 1, want: true},
		{name: "npm registry EAI_AGAIN", command: "npm", args: []string{"install"}, exit: 1, want: true,
			stderr: "npm error code EAI_AGAIN\nnpm error syscall getaddrinfo\nnpm error errno EAI_AGAIN\nnpm error request to https://registry.npmjs.org/x failed, reason: getaddrinfo EAI_AGAIN registry.npmjs.org"},
		{name: "npm registry ECONNREFUSED", command: "npm", args: []string{"install"}, exit: 1, want: true,
			stderr: "npm error code ECONNREFUSED\nnpm error syscall connect\nnpm error errno ECONNREFUSED\nnpm error FetchError: request to https://registry.npmjs.org/x failed, reason: connect ECONNREFUSED 10.0.2.2:443"},
		{name: "npm 9 registry ETIMEDOUT", command: "npm", args: []string{"install"}, exit: 1, want: true,
			stderr: "npm ERR! code ETIMEDOUT\nnpm ERR! errno ETIMEDOUT\nnpm ERR! network request to https://registry.npmjs.org/left-pad failed, reason: connect ETIMEDOUT 104.16.6.34:443"},
		{name: "npm failed request line only", command: "npm", args: []string{"install"}, exit: 1, want: true,
			stderr: "npm error request to https://registry.npmjs.org/x failed, reason: getaddrinfo EAI_AGAIN registry.npmjs.org"},
		{name: "npm certificate failure", command: "npm", args: []string{"install"}, exit: 1,
			stderr: "npm error code UNABLE_TO_VERIFY_LEAF_SIGNATURE\nnpm error request to https://registry.npmjs.org/x failed, reason: unable to verify the first certificate"},
		{name: "pip index", command: "pip", args: []string{"install", "--user", "-r", "requirements.txt"}, stderr: pipIndexUnreachable, exit: 1, want: true},
		{name: "pip build requirements", command: "pip", args: []string{"install", "--user", "-e", "."}, stderr: pipBuildDepsUnreachable, exit: 1, want: true},
		{name: "pip 24 build requirements", command: "pip", args: []string{"install", "--user", "-e", "."}, stderr: pip24BuildDepsUnreachable, exit: 1, want: true},
		{name: "go module proxy", command: "go", args: []string{"mod", "tidy"}, exit: 1, want: true,
			stderr: "go: finding module for package rsc.io/quote\ngo: example.com/fv imports\n\trsc.io/quote: rsc.io/quote@v1.5.2: Get \"https://proxy.golang.org/rsc.io/quote/@v/v1.5.2.zip\": dial tcp 142.251.34.209:443: i/o timeout"},
		{name: "go checksum database", command: "go", args: []string{"mod", "tidy"}, exit: 1, want: true,
			stderr: "go: rsc.io/quote@v1.5.2: verifying module: rsc.io/quote@v1.5.2: Get \"https://sum.golang.org/lookup/rsc.io/quote@v1.5.2\": dial tcp: lookup sum.golang.org on 10.0.2.3:53: dial udp 10.0.2.3:53: connect: network is unreachable"},
		{name: "cargo index", command: "cargo", args: []string{"build"}, stderr: cargoIndexUnreachable, exit: 101, want: true},
		{name: "cargo crate download", command: "cargo", args: []string{"build"}, exit: 101, want: true,
			stderr: "  Downloaded itoa v1.0.18\nerror: failed to download from `https://static.crates.io/crates/itoa/1.0.18/download`\n\nCaused by:\n  [28] Timeout was reached (Connection timed out after 30001 milliseconds)"},
		{name: "cargo replaced registry", command: "cargo", args: []string{"build"}, exit: 101, want: true,
			stderr: "    Updating `mirror` index\nwarning: spurious network error (1 try remaining): [7] Could not connect to server (Failed to connect to 127.0.0.1 port 9 after 0 ms: Could not connect to server)\nerror: failed to get `itoa` as a dependency of package `fvdep v0.1.0 (/tmp/ws)`\n\nCaused by:\n  failed to query replaced source registry `crates-io`\n\nCaused by:\n  download of config.json failed\n\nCaused by:\n  failed to download from `http://127.0.0.1:9/index/config.json`\n\nCaused by:\n  [7] Could not connect to server (Failed to connect to 127.0.0.1 port 9 after 0 ms: Could not connect to server)"},

		// Output the project shapes stays a candidate failure.
		{name: "rustc quotes network words", command: "cargo", args: []string{"build"}, stderr: rustcQuotesNetworkWords, exit: 101},
		{name: "compile_error mimics cargo", command: "cargo", args: []string{"build"}, stderr: rustcCompileErrorMimicsCargo, exit: 101},
		{name: "cargo git dependency", command: "cargo", args: []string{"build"}, stderr: cargoGitDependencyUnreachable, exit: 101},
		{name: "npm invalid spec quoting network words", command: "npm", args: []string{"install"}, exit: 1,
			stderr: "npm error code EINVALIDTAGNAME\nnpm error Invalid tag name \"connection refused\" of package \"left-pad@connection refused\": Tags may not have any characters that encodeURIComponent encodes."},
		{name: "npm URL dependency the manifest names", command: "npm", args: []string{"install"}, manifest: []ExtractedFile{urlDependency}, exit: 1,
			stderr: "npm ERR! code ENOTFOUND\nnpm ERR! syscall getaddrinfo\nnpm ERR! errno ENOTFOUND\nnpm ERR! network request to https://nonexistent.invalid/y-1.0.0.tgz failed, reason: getaddrinfo ENOTFOUND nonexistent.invalid"},
		{name: "pip backend output quoting network words", command: "pip", args: []string{"install", "--user", "-e", "."}, exit: 1,
			stderr: "  Getting requirements to build editable: started\n  Getting requirements to build editable: finished with status 'error'\n  error: subprocess-exited-with-error\n  \n  × Getting requirements to build editable did not run successfully.\n  │ exit code: 1\n  ╰─> [4 lines of output]\n      WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NewConnectionError(x)': /simple/a/\n      ERROR: No matching distribution found for a\n      Traceback (most recent call last):\n      OSError: [Errno -3] Temporary failure in name resolution\n      [end of output]\n  \n  note: This error originates from a subprocess, and is likely not a problem with pip.\nerror: subprocess-exited-with-error"},
		{name: "pip direct URL requirement", command: "pip", args: []string{"install", "--user", "-r", "requirements.txt"}, exit: 2,
			stderr: "Collecting six @ https://files.example.invalid/six-1.16.0-py2.py3-none-any.whl\n  WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NameResolutionError(\"HTTPSConnection(host='files.example.invalid', port=443): Failed to resolve 'files.example.invalid' ([Errno -2] Name or service not known)\")': /six-1.16.0-py2.py3-none-any.whl\nERROR: Exception:\nTraceback (most recent call last):\nsocket.gaierror: [Errno -2] Name or service not known"},
		{name: "go proxy quotes a module path lookup", command: "go", args: []string{"mod", "tidy"}, exit: 1,
			stderr: "go: example.invalid/x@v1.0.0: reading https://proxy.golang.org/example.invalid/x/@v/v1.0.0.info: 404 Not Found\n\tserver response: not found: example.invalid/x@v1.0.0: unrecognized import path \"example.invalid/x\": https fetch: Get \"https://example.invalid/x?go-get=1\": dial tcp: lookup example.invalid on 8.8.8.8:53: no such host"},
		{name: "go malformed import path", command: "go", args: []string{"mod", "tidy"}, exit: 1,
			stderr: "go: example.com/m imports\n\tGet \\\"https://proxy.golang.org/x/@v/list\\\": dial tcp: malformed import path \"Get \\\"https://proxy.golang.org/x/@v/list\\\": dial tcp\": invalid char ' '"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// makewand's own connectivity check confirms each suspected outage.
			stubRegistryCheck(t, unreachableRegistry)
			project := newHostDepsProject(t, tc.manifest...)
			fakeToolPrinting(t, tc.command, tc.stderr, tc.exit)
			result, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: tc.command, Args: tc.args})
			if got := errors.Is(err, ErrDependencyNetworkUnavailable); got != tc.want {
				t.Fatalf("registry outage = %v (err = %v), want %v", got, err, tc.want)
			}
			if !tc.want && err != nil {
				t.Fatalf("err = %v, want the plain failing result", err)
			}
			if result == nil || result.ExitCode != tc.exit {
				t.Fatalf("result = %+v, want exit %d", result, tc.exit)
			}
		})
	}
}

// The TUI deps step for Rust is `cargo build`: broken candidate code whose
// diagnostics quote network phrases must come back as a failing result (so it
// is penalised and auto-fixed), not as a registry outage that stops the build.
func TestRestrictedCargoBuildCompileErrorIsCandidateFailure_FixN1R2(t *testing.T) {
	project := newHostDepsProject(t,
		ExtractedFile{Path: "Cargo.toml", Content: "[package]\nname = \"fv\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"},
		ExtractedFile{Path: "src/main.rs", Content: "fn main() {\n    let retries: u32 = \"dial tcp failed: connection refused\";\n}\n"},
	)
	fakeToolPrinting(t, "cargo", rustcQuotesNetworkWords, 101)
	plan, err := project.DetectInstallPlan()
	if err != nil || plan == nil || plan.Command != "cargo" {
		t.Fatalf("plan = %+v, err = %v", plan, err)
	}
	result, err := project.RunRestrictedPlan(context.Background(), *plan)
	if err != nil {
		t.Fatalf("err = %v, want the compile failure as a plain result", err)
	}
	if result == nil || result.ExitCode != 101 {
		t.Fatalf("result = %+v", result)
	}
}

// fakeHeldBwrap emulates bwrap for the user-mode network: with --info-fd it
// reports its PID and waits on --block-fd. Like npm, the sandboxed install
// fails at once when npm retries are off, and only after its retries (about
// 70 seconds; 20 here) otherwise - unless okWhenOffline makes it an install
// that needs no download.
func fakeHeldBwrap(logPath string, okWhenOffline bool) string {
	offline := `echo "npm error code ENOTFOUND" >&2
  echo "npm error network request to https://registry.npmjs.org/left-pad failed, reason: getaddrinfo ENOTFOUND registry.npmjs.org" >&2
  exit 1`
	if okWhenOffline {
		offline = `echo "up to date"; exit 0`
	}
	return fmt.Sprintf(`log=%q
info=; block=; retries=default
while [ $# -gt 0 ]; do
  case "$1" in
    --info-fd) info=$2; shift 2 ;;
    --block-fd) block=$2; shift 2 ;;
    --ro-bind-data) shift 3 ;;
    --setenv) [ "$2" = npm_config_fetch_retries ] && retries=$3; shift 3 ;;
    *) shift ;;
  esac
done
echo "launch info=${info:-none} retries=$retries" >> "$log"
if [ -n "$info" ]; then
  eval "printf '{\"child-pid\": %%d}\n' $$ >&$info"
  eval "exec $info>&-"
  eval "read -r unblocked <&$block"
fi
if [ "$retries" = 0 ]; then
  %s
fi
/bin/sleep 20
echo "npm error code ENOTFOUND" >&2
exit 1
`, logPath, offline)
}

type heldBwrapEnv struct {
	log string
}

// installHeldBwrap installs fakeHeldBwrap as the verification sandbox and slirpBody
// as the trusted slirp4netns.
func installHeldBwrap(t *testing.T, okWhenOffline bool, slirpBody string) heldBwrapEnv {
	t.Helper()
	fakeWorkingBwrap(t)
	swapSeccompVars(t, "linux", false, nil)
	dir := t.TempDir()
	env := heldBwrapEnv{log: filepath.Join(dir, "launches.log")}
	bwrap := filepath.Join(dir, "bwrap")
	writeExecScript(t, bwrap, fakeHeldBwrap(env.log, okWhenOffline))
	verifyLookPath = func(string) (string, error) { return bwrap, nil }
	slirp := filepath.Join(dir, "slirp4netns")
	writeExecScript(t, slirp, slirpBody)
	resolv := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 127.0.0.53\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldLookPath, oldResolv := userNetLookPath, userNetResolvConf
	userNetLookPath = func(name string) (string, error) {
		if name != userNetHelperName {
			return "", fmt.Errorf("%w: %s", trustedbin.ErrNotFound, name)
		}
		return slirp, nil
	}
	userNetResolvConf = resolv
	t.Cleanup(func() { userNetLookPath, userNetResolvConf = oldLookPath, oldResolv })
	// The fake sandbox has no namespaces of its own: stand-ins let the launch
	// reach slirp4netns, whose failure is what these tests exercise.
	fakeUserNetConfiguration(t)
	return env
}

const slirpCannotOpenTun = "echo 'slirp4netns: open(\"/dev/net/tun\"): Operation not permitted' >&2\nexit 1\n"

// slirp4netns installed but unable to attach: the install must not run on
// with npm's retries against a network that is not there (71 s), and the
// error must report the attach failure instead of advice to install it.
func TestDependencyInstallHelperAttachFailureFailsFast_FixN1R2(t *testing.T) {
	env := installHeldBwrap(t, false, slirpCannotOpenTun)
	project, err := NewProject("usernet-attach-fail", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	declareNpmRegistryDependency(t, project.Path)
	start := time.Now()
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}})
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("took %v: the install ran on with npm retries after the helper failed to attach", elapsed)
	}
	if !errors.Is(err, ErrDependencyNetworkUnavailable) {
		t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable", err)
	}
	msg := err.Error()
	if !regexp.MustCompile(`slirp4netns is installed but could not be attached to the sandbox \(\S.*\), so the dependency install ran without outbound network`).MatchString(msg) {
		t.Fatalf("err = %v, want the attach failure and its cause", err)
	}
	if strings.Contains(msg, "apt install") || strings.Contains(msg, "dnf install") {
		t.Fatalf("err = %v, tells the user to install a helper that is installed", err)
	}
	if result == nil || result.ExitCode != 1 {
		t.Fatalf("result = %+v", result)
	}
	launches := strings.Split(strings.TrimSpace(readLog(t, env.log)), "\n")
	if len(launches) != 2 || launches[0] != "launch info=3 retries=default" || launches[1] != "launch info=none retries=0" {
		t.Fatalf("launches:\n%s\nwant the held launch, then one loopback-only launch with npm retries off", strings.Join(launches, "\n"))
	}
}

// An install that needs no download still succeeds when the helper cannot be
// attached.
func TestDependencyInstallHelperAttachFailureOfflineInstallSucceeds_FixN1R2(t *testing.T) {
	env := installHeldBwrap(t, true, slirpCannotOpenTun)
	project, err := NewProject("usernet-attach-fail-ok", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if got := strings.TrimSpace(readLog(t, env.log)); got != "launch info=3 retries=default\nlaunch info=none retries=0" {
		t.Fatalf("launches:\n%s\nwant the held launch, then one loopback-only launch", got)
	}
}

// fakeCargo stands in for cargo on the host: like the sandbox's fresh
// CARGO_HOME, nothing resolves offline until `cargo fetch` has filled the
// cache.
const fakeCargo = `echo "$*" >> "$FAKE_CARGO_LOG"
fetched="$HOME/.fake-cargo-fetched"
case "$1" in
fetch)
  if [ -n "$FAKE_CARGO_FETCH_STDERR" ]; then
    printf '%s\n' "$FAKE_CARGO_FETCH_STDERR" >&2
    exit 101
  fi
  : > "$fetched"; exit 0 ;;
check|test|build)
  if [ ! -f "$fetched" ]; then
    printf '%s\n' '    Updating crates.io index' 'warning: spurious network error (1 try remaining): [6] Could not resolve hostname (Could not resolve host: index.crates.io)' 'error: failed to get ` + "`itoa`" + ` as a dependency of package ` + "`fv v0.1.0`" + `' >&2
    exit 101
  fi
  [ "$1" = test ] && printf '%s\n' "running 1 test" "test tests::renders ... ok" "test result: ok. 1 passed; 0 failed"
  exit 0 ;;
esac
exit 0
`

func newFakeCargoProject(t *testing.T, fetchStderr string) (*Project, string) {
	t.Helper()
	project := newHostDepsProject(t,
		ExtractedFile{Path: "Cargo.toml", Content: "[package]\nname = \"fv\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nitoa = \"1\"\n"},
		ExtractedFile{Path: "Cargo.lock", Content: "version = 4\n"},
		ExtractedFile{Path: "src/main.rs", Content: "fn main() {}\n"},
	)
	dir := t.TempDir()
	writeExecScript(t, filepath.Join(dir, "cargo"), fakeCargo)
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	log := filepath.Join(t.TempDir(), "cargo.log")
	t.Setenv("FAKE_CARGO_LOG", log)
	t.Setenv("FAKE_CARGO_FETCH_STDERR", fetchStderr)
	return project, log
}

// A healthy Rust project with a crates.io dependency and a benign candidate:
// dependencies are fetched by a dependency step before the offline quick
// check and tests, which then pass.
func TestRustDependenciesFetchedBeforeOfflineSteps_FixN1R2(t *testing.T) {
	project, log := newFakeCargoProject(t, "")
	files := []ExtractedFile{{Path: "src/main.rs", Content: "fn main() {}\n// candidate\n"}}
	report, err := project.EvaluateCandidateFiles(context.Background(), files)
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if !report.Passed || report.EnvironmentError {
		t.Fatalf("Passed=%v EnvironmentError=%v QuickCheckError=%q TestsError=%q, want a pass", report.Passed, report.EnvironmentError, report.QuickCheckError, report.TestsError)
	}
	if len(report.QuickCheckPlans) == 0 || report.QuickCheckPlans[0].Kind != "deps" || report.QuickCheckPlans[0].DisplayCommand() != "cargo fetch" {
		t.Fatalf("QuickCheckPlans = %+v, want cargo fetch as a dependency step first", report.QuickCheckPlans)
	}
	// The committed Cargo.lock is honoured: every cargo step runs --locked.
	if got := strings.TrimSpace(readLog(t, log)); got != "fetch --locked\ncheck --tests --locked\ntest --locked" {
		t.Fatalf("cargo invocations = %q, want fetch, then the quick check, then tests", got)
	}
}

// A failed fetch is an environment error when the registry is unreachable,
// and a candidate failure otherwise.
func TestRustDependencyFetchFailureAttribution_FixN1R2(t *testing.T) {
	files := []ExtractedFile{{Path: "src/main.rs", Content: "fn main() {}\n// candidate\n"}}
	t.Run("registry unreachable", func(t *testing.T) {
		stubRegistryCheck(t, unreachableRegistry)
		project, _ := newFakeCargoProject(t, cargoIndexUnreachable)
		report, err := project.EvaluateCandidateFiles(context.Background(), files)
		if err != nil {
			t.Fatalf("EvaluateCandidateFiles: %v", err)
		}
		if report.Passed || !report.EnvironmentError || !strings.Contains(report.QuickCheckError, "could not reach the network") {
			t.Fatalf("Passed=%v EnvironmentError=%v QuickCheckError=%q, want an environment error", report.Passed, report.EnvironmentError, report.QuickCheckError)
		}
		attempt := CandidateAttempt{Provider: "claude", Requested: "claude", Files: files, Content: RenderExtractedFiles(files), Verification: report}
		if ShouldRecordCandidateQuality(attempt) || CandidateAttemptStage(context.Background(), attempt) != CandidateProgressUnverified {
			t.Fatal("a registry outage must leave the candidate unverified, not rejected")
		}
	})
	t.Run("missing crate version", func(t *testing.T) {
		project, _ := newFakeCargoProject(t, "    Updating crates.io index\nerror: failed to select a version for the requirement `itoa = \"^99\"`\ncandidate versions found which didn't match: 1.0.18, 1.0.17")
		report, err := project.EvaluateCandidateFiles(context.Background(), files)
		if err != nil {
			t.Fatalf("EvaluateCandidateFiles: %v", err)
		}
		if report.Passed || report.EnvironmentError || !strings.Contains(report.QuickCheckError, "failed to select a version") {
			t.Fatalf("Passed=%v EnvironmentError=%v QuickCheckError=%q, want a candidate failure", report.Passed, report.EnvironmentError, report.QuickCheckError)
		}
	})
}

// fakeRecordingBwrap emulates a sandbox that runs every command successfully
// and records, for a pip step, the distutils configuration in the sandbox
// HOME.
func fakeRecordingBwrap(logPath string) string {
	return fmt.Sprintf(`log=%q
home=; cmd=
while [ $# -gt 0 ]; do
  case "$1" in
    --setenv) [ "$2" = HOME ] && home=$3; shift 3 ;;
    --chdir|--tmpfs|--proc|--dev) shift 2 ;;
    --bind|--ro-bind) shift 3 ;;
    --*) shift ;;
    *) cmd=$1; break ;;
  esac
done
if [ "$cmd" = pip ]; then
  if [ -f "$home/.pydistutils.cfg" ]; then
    echo "pip config: $(/bin/cat "$home/.pydistutils.cfg" | /usr/bin/tr '\n' ';')" >> "$log"
  else
    echo "pip config: none" >> "$log"
  fi
fi
[ "$cmd" = pytest ] && echo "1 passed"
exit 0
`, logPath)
}

// A sandboxed install of a pyproject.toml project keeps setuptools' egg-info
// out of the workspace: the sandbox HOME carries a distutils configuration
// pointing egg_base at the sandbox /tmp for the install, and only then.
func TestSandboxedPipInstallRedirectsEggInfo_FixN1R2(t *testing.T) {
	cases := []struct {
		name     string
		manifest ExtractedFile
		want     string
	}{
		{"pyproject", ExtractedFile{Path: "pyproject.toml", Content: "[project]\nname = \"fvpkg\"\nversion = \"0.1.0\"\n\n[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\n"}, "pip config: [egg_info];egg_base = /tmp;"},
		{"requirements only", ExtractedFile{Path: "requirements.txt", Content: "six==1.16.0\n"}, "pip config: none"},
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
			writeExecScript(t, bwrap, fakeRecordingBwrap(log))
			verifyLookPath = func(string) (string, error) { return bwrap, nil }
			project, err := NewProject("egg-info", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			files := []ExtractedFile{tc.manifest, {Path: "fvpkg/__init__.py", Content: "def f():\n    return 1\n"}, {Path: "tests/test_f.py", Content: "from fvpkg import f\n\ndef test_f():\n    assert f() == 1\n"}}
			if err := project.WriteFiles(files); err != nil {
				t.Fatal(err)
			}
			report := project.VerifyRestrictedWorkspace(context.Background(), files[1:2])
			if report.DepsError != "" || report.IntegrityError != "" {
				t.Fatalf("DepsError=%q IntegrityError=%q", report.DepsError, report.IntegrityError)
			}
			// The dependency step is split into a networked wheel download and
			// an offline install; both run with the same sandbox HOME, so both
			// see the egg-info redirect (it only matters for the install).
			lines := strings.Split(strings.TrimSpace(readLog(t, log)), "\n")
			if len(lines) != 2 {
				t.Fatalf("pip steps logged %d lines, want a download and an install:\n%s", len(lines), strings.Join(lines, "\n"))
			}
			for _, line := range lines {
				if line != tc.want {
					t.Fatalf("pip step saw %q, want %q", line, tc.want)
				}
			}
			if _, err := os.Lstat(filepath.Join(project.Path, filepath.FromSlash(sandboxHomeRelPath), ".pydistutils.cfg")); !os.IsNotExist(err) {
				t.Fatalf("distutils configuration left behind after the install: %v", err)
			}
		})
	}
}

// Live: real setuptools, asked for build requirements the way pip asks it,
// in the real sandbox, writes no egg-info into the verified workspace.
func TestSandboxedSetuptoolsWritesNoEggInfoIntoWorkspace_FixN1R2(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	if err := exec.Command("/usr/bin/python3", "-I", "-c", "import setuptools.build_meta").Run(); err != nil {
		t.Skipf("system python3 has no setuptools: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	project, err := NewProject("egg-info-live", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(project.Path, "tools")
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	files := []ExtractedFile{
		{Path: "pyproject.toml", Content: "[build-system]\nrequires = [\"setuptools>=61\"]\nbuild-backend = \"setuptools.build_meta\"\n\n[project]\nname = \"fvpkg\"\nversion = \"0.1.0\"\n\n[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\n"},
		{Path: "fvpkg/__init__.py", Content: "def f():\n    return 1\n"},
		{Path: "tests/test_f.py", Content: "from fvpkg import f\n\ndef test_f():\n    assert f() == 1\n"},
	}
	if err := project.WriteFiles(files); err != nil {
		t.Fatal(err)
	}
	// pip runs this hook in its build environment before the editable build.
	writeExecScript(t, filepath.Join(tools, "pip"), "exec /usr/bin/python3 -c 'import setuptools.build_meta as b; b.get_requires_for_build_editable()'\n")
	writeExecScript(t, filepath.Join(tools, "pytest"), "echo '1 passed'\n")
	report := project.VerifyRestrictedWorkspace(context.Background(), files[1:2])
	if report.IntegrityError != "" || report.DepsError != "" || !report.Isolated {
		t.Fatalf("Isolated=%v IntegrityError=%q DepsError=%q", report.Isolated, report.IntegrityError, report.DepsError)
	}
	if _, err := os.Stat(filepath.Join(project.Path, "fvpkg.egg-info")); !os.IsNotExist(err) {
		t.Fatalf("egg-info written into the workspace: %v", err)
	}
}
