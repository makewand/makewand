//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/trustedbin"
)

// stubRegistryCheck replaces makewand's registry connectivity check with one
// that returns check, and records the command of every check it runs.
func stubRegistryCheck(t *testing.T, check registryCheck) *[]string {
	t.Helper()
	var commands []string
	old := checkRegistry
	checkRegistry = func(_ context.Context, _ verifyExecEnvironment, command string) registryCheck {
		commands = append(commands, command)
		return check
	}
	t.Cleanup(func() { checkRegistry = old })
	return &commands
}

// unreachableRegistry is a registry check that got no response.
var unreachableRegistry = registryCheck{done: true, unreachable: []string{"registry.example (no response)"}}

// fakeRegistryProbe stands in for makewand's own executable as the registry
// connectivity probe: it answers the first URL of every chain it is asked
// (without its markers) with verdict ("reachable" or "unreachable") and logs
// its working directory, the directory's contents, its first argument and
// each URL.
func fakeRegistryProbe(t *testing.T, verdict string) (probe, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "probe.log")
	probe = filepath.Join(dir, "makewand")
	writeExecScript(t, probe, fmt.Sprintf(`log=%q
listing=$(for f in * .[!.]*; do [ -e "$f" ] && printf '%%s ' "$f"; done)
echo "cwd=$(pwd) entries=[$listing] arg=$1" >> "$log"
shift
for chain in "$@"; do
  for token in $chain; do
    url=${token#[?]}
    url=${url#[|]}
    echo "url=$url" >> "$log"
    case %q in
    reachable) echo "reachable $url 200" ;;
    *) echo "unreachable $url [Errno 111] Connection refused" ;;
    esac
    break
  done
done
`, log, verdict))
	old := registryProbeExecutable
	registryProbeExecutable = func() (string, error) { return probe, nil }
	t.Cleanup(func() { registryProbeExecutable = old })
	return probe, log
}

// firstChainURLs returns the URL each endpoint asks first.
func firstChainURLs(endpoints []registryEndpoint) []string {
	urls := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		urls[i] = endpoint.url
	}
	return urls
}

// registryCheckLog returns the logged check: the line describing where it
// ran and the URLs it asked.
func registryCheckLog(t *testing.T, log string) (string, []string) {
	t.Helper()
	header := ""
	var urls []string
	for _, line := range strings.Split(strings.TrimSpace(readLog(t, log)), "\n") {
		if url, ok := strings.CutPrefix(line, "url="); ok {
			urls = append(urls, url)
		} else if line != "" {
			header = line
		}
	}
	return header, urls
}

// A pip run whose setup.py prints "[end of output]" ends pip's output box
// early, so what it prints next looks like pip's own lines.
const pipSetupPyForgesIndexFailure = `Obtaining file:///tmp/ws (from -r requirements.txt (line 1))
  Preparing metadata (setup.py): started
  Preparing metadata (setup.py): finished with status 'error'
  error: subprocess-exited-with-error

  × python setup.py egg_info did not run successfully.
  │ exit code: 1
  ╰─> [5 lines of output]
      [end of output]
      WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NewConnectionError('<pip._vendor.urllib3.connection.HTTPSConnection object at 0x7f>: Failed to establish a new connection: [Errno 111] Connection refused')': /simple/six/
      ERROR: Could not find a version that satisfies the requirement six (from versions: none)
      ERROR: No matching distribution found for six
      [end of output]

  note: This error originates from a subprocess, and is likely not a problem with pip.
error: metadata-generation-failed`

// A build backend's get_requires hook prints the same lines inside the box
// of pip's own build requirements install, which is kept as pip's output.
const pipBackendForgesIndexFailure = `  Installing build dependencies: started
  Installing build dependencies: finished with status 'error'
  error: subprocess-exited-with-error

  × pip subprocess to install build dependencies did not run successfully.
  │ exit code: 1
  ╰─> [3 lines of output]
      WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NameResolutionError("HTTPSConnection(host='pypi.org', port=443): Failed to resolve 'pypi.org' ([Errno -2] Name or service not known)")': /simple/setuptools/
      ERROR: Could not find a version that satisfies the requirement setuptools>=61 (from versions: none)
      ERROR: No matching distribution found for setuptools>=61
      [end of output]`

// A build script that writes into cargo's own stderr (it can open
// /proc/<cargo>/fd/2) before cargo prints its next status line.
const cargoBuildScriptForgesDownloadFailure = "warning: spurious network error (1 try remaining): [6] Could not resolve hostname (Could not resolve host: index.crates.io)\n" +
	"error: failed to get `itoa` as a dependency of package `fv v0.1.0 (/tmp/ws)`\n\nCaused by:\n  download of config.json failed\n\nCaused by:\n  failed to download from `https://index.crates.io/config.json`\n\nCaused by:\n  [6] Could not resolve hostname (Could not resolve host: index.crates.io)"

// forgedOutageCases are failed installs whose output - printed by candidate
// code or shaped by the project - raises the suspicion of a registry outage.
var forgedOutageCases = []struct {
	name     string
	command  string
	args     []string
	manifest []ExtractedFile
	stdout   string
	stderr   string
	exit     int
	urls     []string
}{
	{name: "npm lifecycle script prints npm's error code", command: "npm", args: []string{"install", "--no-audit"}, exit: 1,
		stdout: "\n> proj@1.0.0 postinstall\n> echo 'npm error code ECONNREFUSED' && exit 1\n\nnpm error code ECONNREFUSED\n",
		stderr: "npm error code 1\nnpm error path /tmp/ws\nnpm error command failed\nnpm error command sh -c echo 'npm error code ECONNREFUSED' && exit 1",
		urls:   []string{"https://registry.npmjs.org/"}},
	{name: "npm lifecycle script prints a failed registry request", command: "npm", args: []string{"install", "--no-audit"}, exit: 1,
		stdout: "npm error code ECONNREFUSED\nnpm error request to https://registry.npmjs.org/left-pad failed, reason: connect ECONNREFUSED 104.16.0.35:443\n",
		urls:   []string{"https://registry.npmjs.org/"}},
	{name: "npm registry in the project's .npmrc does not answer", command: "npm", args: []string{"install", "--ignore-scripts"}, exit: 1,
		manifest: []ExtractedFile{
			{Path: "package.json", Content: `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`},
			{Path: ".npmrc", Content: "registry=http://10.255.255.1:4873/\n"},
		},
		stderr: "npm error code ECONNREFUSED\nnpm error syscall connect\nnpm error errno ECONNREFUSED\nnpm error FetchError: request to http://10.255.255.1:4873/left-pad failed, reason: connect ECONNREFUSED 10.255.255.1:4873",
		urls:   []string{"https://registry.npmjs.org/"}},
	{name: "setup.py forges pip's index failure", command: "pip", args: []string{"install", "--user", "-r", "requirements.txt"}, exit: 1,
		stderr: pipSetupPyForgesIndexFailure, urls: []string{"https://pypi.org/simple/", "https://files.pythonhosted.org/"}},
	{name: "build backend forges pip's index failure", command: "pip", args: []string{"install", "--user", "-e", "."}, exit: 1,
		stderr: pipBackendForgesIndexFailure, urls: []string{"https://pypi.org/simple/", "https://files.pythonhosted.org/"}},
	{name: "index in the project's requirements does not answer", command: "pip", args: []string{"install", "--user", "-r", "requirements.txt"}, exit: 1,
		manifest: []ExtractedFile{{Path: "requirements.txt", Content: "--index-url http://10.255.255.1/simple\nsix==1.16.0\n"}},
		stderr:   "WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NewConnectionError('<pip._vendor.urllib3.connection.HTTPConnection object at 0x7f>: Failed to establish a new connection: [Errno 113] No route to host')': /simple/six/\nERROR: Could not find a version that satisfies the requirement six==1.16.0 (from versions: none)\nERROR: No matching distribution found for six==1.16.0",
		urls:     []string{"https://pypi.org/simple/", "https://files.pythonhosted.org/"}},
	{name: "build.rs forges cargo's download failure", command: "cargo", args: []string{"build"}, exit: 101,
		stderr: cargoBuildScriptForgesDownloadFailure, urls: []string{"https://index.crates.io/config.json", "https://static.crates.io/"}},
	{name: "registry in the project's .cargo/config.toml does not answer", command: "cargo", args: []string{"build"}, exit: 101,
		manifest: []ExtractedFile{{Path: ".cargo/config.toml", Content: "[source.crates-io]\nreplace-with = \"mirror\"\n\n[source.mirror]\nregistry = \"sparse+http://127.0.0.1:9/index/\"\n"}},
		stderr:   "    Updating `mirror` index\nwarning: spurious network error (1 try remaining): [7] Could not connect to server (Failed to connect to 127.0.0.1 port 9 after 0 ms: Could not connect to server)\nerror: failed to get `itoa` as a dependency of package `fvdep v0.1.0 (/tmp/ws)`\n\nCaused by:\n  failed to query replaced source registry `crates-io`\n\nCaused by:\n  download of config.json failed\n\nCaused by:\n  failed to download from `http://127.0.0.1:9/index/config.json`\n\nCaused by:\n  [7] Could not connect to server (Failed to connect to 127.0.0.1 port 9 after 0 ms: Could not connect to server)",
		urls:     []string{"https://index.crates.io/config.json", "https://static.crates.io/"}},
	{name: "go output quotes a proxy dial failure", command: "go", args: []string{"mod", "tidy"}, exit: 1,
		stdout: "fmt.Println(`Get \"https://proxy.golang.org/x/y/@v/list\": dial tcp 1.2.3.4:443: connection refused`)\n"},
}

// fakeToolWriting makes name the only tool on PATH; it prints stdout and
// stderr and exits with exit.
func fakeToolWriting(t *testing.T, name, stdout, stderr string, exit int) {
	t.Helper()
	dir := t.TempDir()
	writeExecScript(t, filepath.Join(dir, name), fmt.Sprintf("/bin/cat <<'MAKEWAND_OUT'\n%s\nMAKEWAND_OUT\n/bin/cat >&2 <<'MAKEWAND_EOF'\n%s\nMAKEWAND_EOF\nexit %d\n", stdout, stderr, exit))
	t.Setenv("PATH", dir)
}

// What a failed install prints - including output its candidate code or its
// project configuration produces - only raises the suspicion of a registry
// outage. makewand's own check of the ecosystem's default registry decides:
// when the registry answers, the failure is the candidate's, however
// convincing the output.
func TestForgedRegistryOutageIsCandidateFailure_R3(t *testing.T) {
	for _, tc := range forgedOutageCases {
		t.Run(tc.name, func(t *testing.T) {
			project := newHostDepsProject(t, tc.manifest...)
			_, log := fakeRegistryProbe(t, "reachable")
			fakeToolWriting(t, tc.command, tc.stdout, tc.stderr, tc.exit)
			plan := ExecPlan{Kind: "deps", Command: tc.command, Args: tc.args}
			if evidence := project.registryFailureEvidence(plan, &ExecResult{ExitCode: tc.exit, Stdout: tc.stdout, Stderr: tc.stderr}); evidence == "" {
				t.Fatal("the output does not raise the suspicion of an outage; the case tests nothing")
			}
			result, err := project.RunRestrictedPlan(context.Background(), plan)
			if err != nil {
				t.Fatalf("err = %v, want the candidate's failure as a plain result", err)
			}
			if result == nil || result.ExitCode != tc.exit {
				t.Fatalf("result = %+v, want exit %d", result, tc.exit)
			}
			header, urls := registryCheckLog(t, log)
			want := tc.urls
			if want == nil {
				want = firstChainURLs(registryCheckEndpoints(tc.command))
			}
			if !slices.Equal(urls, want) {
				t.Fatalf("connectivity check asked %q, want the default registry %q (%s)", urls, want, header)
			}
			if !strings.Contains(header, " entries=[] arg="+registryProbeArg) || strings.Contains(header, "cwd="+project.Path) {
				t.Fatalf("check ran as %q: want makewand's own probe in an empty directory, not the project", header)
			}
			if result.Note == "" || !strings.Contains(result.Note, "makewand's own connectivity check reached") {
				t.Fatalf("Note = %q, want why the output was not trusted", result.Note)
			}
		})
	}
}

// The same outputs with the default registry not answering makewand's check
// are environment errors, and the guidance reports the check's result.
func TestGenuineRegistryOutageIsEnvironmentError_R3(t *testing.T) {
	for _, tc := range forgedOutageCases {
		t.Run(tc.name, func(t *testing.T) {
			project := newHostDepsProject(t, tc.manifest...)
			_, log := fakeRegistryProbe(t, "unreachable")
			fakeToolWriting(t, tc.command, tc.stdout, tc.stderr, tc.exit)
			result, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: tc.command, Args: tc.args})
			if !errors.Is(err, ErrDependencyNetworkUnavailable) {
				t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable", err)
			}
			if result == nil || result.ExitCode != tc.exit {
				t.Fatalf("result = %+v, want exit %d alongside the error", result, tc.exit)
			}
			_, urls := registryCheckLog(t, log)
			if len(urls) == 0 {
				t.Fatal("no connectivity check ran")
			}
			if want := "makewand's own connectivity check could not reach " + urlHost(urls[0]) + " ([Errno 111] Connection refused)"; !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "from this host either") {
				t.Fatalf("err = %v, want %q from this host", err, want)
			}
		})
	}
}

// A dependency step without outbound network (no slirp4netns here) reaches
// no registry, so a suspected outage of an install that needs one (it
// declares a registry dependency that is not installed) is an environment
// error without a connectivity check.
func TestOutageWithoutOutboundNetworkNeedsNoCheck_R3(t *testing.T) {
	installFakeUserNet(t, fakeUserNetOptions{
		bwrapTail: "echo \"npm error code ENOTFOUND\" >&2\nexit 1",
		missing:   []string{userNetHelperName},
	})
	_, log := fakeRegistryProbe(t, "reachable")
	project, err := NewProject("r3-offline", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project.Path, "package.json"), `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`)
	_, err = project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if !errors.Is(err, ErrDependencyNetworkUnavailable) || !strings.Contains(err.Error(), "slirp4netns was not found") {
		t.Fatalf("err = %v, want the missing-helper environment error", err)
	}
	if got := readLog(t, log); got != "" {
		t.Fatalf("a connectivity check ran for a step without outbound network:\n%s", got)
	}
}

// When the connectivity check cannot run (here makewand's own executable is
// not available to it) nothing confirms the outage, and the output alone
// never makes it an environment error.
func TestUnconfirmedOutageIsCandidateFailure_R3(t *testing.T) {
	project := newHostDepsProject(t)
	old := registryProbeExecutable
	registryProbeExecutable = func() (string, error) { return "", errors.New("makewand's own executable could not be located: test") }
	t.Cleanup(func() { registryProbeExecutable = old })
	fakeToolWriting(t, "npm", "", npmRegistryENOTFOUND, 1)
	result, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if err != nil || result == nil || result.ExitCode != 1 {
		t.Fatalf("result = %+v, err = %v, want the plain failing result", result, err)
	}
	if !strings.Contains(result.Note, "its own connectivity check could not run (makewand's own executable could not be located: test; tried 2 times)") {
		t.Fatalf("Note = %q, want why the outage stays unconfirmed", result.Note)
	}
}

// A check that runs out of time counts the endpoints that never answered as
// unreachable: nothing the candidate controls can slow it down.
func TestRegistryCheckTimeoutIsUnreachable_R3(t *testing.T) {
	project := newHostDepsProject(t)
	dir := t.TempDir()
	probe := filepath.Join(dir, "makewand")
	writeExecScript(t, probe, "shift\nset -- $1\necho \"reachable $1 200\"\nexec /bin/sleep 30\n")
	oldProbe, oldTimeout := registryProbeExecutable, registryCheckTimeout
	registryProbeExecutable = func() (string, error) { return probe, nil }
	registryCheckTimeout = time.Second
	t.Cleanup(func() { registryProbeExecutable, registryCheckTimeout = oldProbe, oldTimeout })
	fakeToolWriting(t, "pip", "", pipIndexUnreachable, 1)
	start := time.Now()
	_, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the check took %s, want it bounded", elapsed)
	}
	if !errors.Is(err, ErrDependencyNetworkUnavailable) || !strings.Contains(err.Error(), "files.pythonhosted.org (no response within 1s)") || strings.Contains(err.Error(), "pypi.org (") {
		t.Fatalf("err = %v, want only the endpoint that never answered reported unreachable", err)
	}
}

// fakeRunningBwrap emulates bwrap for the registry check: it logs each launch
// (whether it got the user-mode network, its working directory and command),
// holds a user-network launch until it is released, and then runs the
// command from that directory - except npm (the bare name, or the trusted
// executable a networked fetch runs), which fails like an install whose
// registry did not answer.
func fakeRunningBwrap(logPath string) string {
	return fmt.Sprintf(`log=%q
info=; block=; chdir=
while [ $# -gt 0 ]; do
  case "$1" in
    --info-fd) info=$2; shift 2 ;;
    --block-fd) block=$2; shift 2 ;;
    --chdir) chdir=$2; shift 2 ;;
    --ro-bind-data|--setenv|--bind|--ro-bind) shift 3 ;;
    --tmpfs|--proc|--dev|--seccomp|--dir) shift 2 ;;
    --*) shift ;;
    *) break ;;
  esac
done
echo "launch info=${info:-none} chdir=$chdir cmd=$1" >> "$log"
if [ -n "$info" ]; then
  eval "printf '{\"child-pid\": %%d}\n' $$ >&$info"
  eval "exec $info>&-"
  eval "read -r unblocked <&$block"
fi
case "$1" in npm|*/npm)
  printf '%%s\n' 'npm error code ENOTFOUND' 'npm error network request to https://registry.npmjs.org/left-pad failed, reason: getaddrinfo ENOTFOUND registry.npmjs.org' >&2
  exit 1 ;;
esac
cd "$chdir" && exec "$@"
`, logPath)
}

// installRunningUserNet installs fakeRunningBwrap as the verification sandbox
// with a fake slirp4netns that attaches.
func installRunningUserNet(t *testing.T) string {
	t.Helper()
	fakeWorkingBwrap(t)
	stubNpmVersion(t, npmNetworkedLockMajor) // see installFakeUserNet
	swapSeccompVars(t, "linux", false, nil)
	dir := t.TempDir()
	log := filepath.Join(dir, "launches.log")
	bwrap := filepath.Join(dir, "bwrap")
	writeExecScript(t, bwrap, fakeRunningBwrap(log))
	verifyLookPath = func(string) (string, error) { return bwrap, nil }
	slirp := filepath.Join(dir, "slirp4netns")
	writeExecScript(t, slirp, fakeSlirp(filepath.Join(dir, "slirp.log")))
	resolv := filepath.Join(dir, "resolv.conf")
	writeFile(t, resolv, "nameserver 127.0.0.53\n")
	oldLookPath, oldResolv := userNetLookPath, userNetResolvConf
	userNetLookPath = func(name string) (string, error) {
		if name == userNetHelperName {
			return slirp, nil
		}
		return "", fmt.Errorf("%w: %s", trustedbin.ErrNotFound, name)
	}
	userNetResolvConf = resolv
	t.Cleanup(func() { userNetLookPath, userNetResolvConf = oldLookPath, oldResolv })
	fakeUserNetConfiguration(t)
	return log
}

// In the sandbox, the check runs in a sandbox of its own with the user-mode
// network the install had, from an empty directory instead of the project.
func TestRegistryCheckRunsInFreshUserModeSandbox_R3(t *testing.T) {
	for _, verdict := range []string{"reachable", "unreachable"} {
		t.Run(verdict, func(t *testing.T) {
			launches := installRunningUserNet(t)
			probe, log := fakeRegistryProbe(t, verdict)
			project, err := NewProject("r3-check-sandbox", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(project.Path, ".npmrc"), "registry=http://10.255.255.1:4873/\n")
			result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
			if result == nil || result.ExitCode != 1 {
				t.Fatalf("result = %+v, want the failed install", result)
			}
			switch verdict {
			case "reachable":
				if err != nil || !strings.Contains(result.Note, "makewand's own connectivity check reached registry.npmjs.org") {
					t.Fatalf("err = %v, Note = %q, want the candidate's failure (the registry answered)", err, result.Note)
				}
			default:
				if !errors.Is(err, ErrDependencyNetworkUnavailable) ||
					!strings.Contains(err.Error(), "makewand's own connectivity check could not reach registry.npmjs.org ([Errno 111] Connection refused) from a fresh verification sandbox with outbound access through slirp4netns either") {
					t.Fatalf("err = %v, want the check's result as an environment error", err)
				}
			}
			lines := strings.Split(strings.TrimSpace(readLog(t, launches)), "\n")
			if len(lines) != 2 || !isFetchDirLaunch(t, lines[0], "npm") {
				t.Fatalf("launches:\n%s\nwant the install, then the check", strings.Join(lines, "\n"))
			}
			checkDir, ok := strings.CutPrefix(lines[1], "launch info=3 chdir=")
			if !ok || !strings.HasSuffix(checkDir, " cmd="+probe) {
				t.Fatalf("check launch %q: want the user-mode network and makewand's own probe", lines[1])
			}
			checkDir = strings.TrimSuffix(checkDir, " cmd="+probe)
			if checkDir == project.Path || !strings.Contains(filepath.Base(checkDir), "makewand-registry-check-") {
				t.Fatalf("check ran in %q, want a fresh directory", checkDir)
			}
			if _, err := os.Stat(checkDir); !os.IsNotExist(err) {
				t.Fatalf("check directory left behind: %v", err)
			}
			header, urls := registryCheckLog(t, log)
			if header != "cwd="+checkDir+" entries=[] arg="+registryProbeArg || !slices.Equal(urls, []string{"https://registry.npmjs.org/"}) {
				t.Fatalf("check %q asked %q, want makewand's own probe in an empty directory asking the default registry", header, urls)
			}
		})
	}
}

// isFetchDirLaunch reports whether a fakeRunningBwrap launch line is a
// networked fetch of command: with the user-mode network, in a fetch directory
// of the dependency fetch area (never the project), running command by name
// or as a trusted executable.
func isFetchDirLaunch(t *testing.T, line, command string) bool {
	t.Helper()
	rest, ok := strings.CutPrefix(line, "launch info=3 chdir=")
	if !ok {
		return false
	}
	dir, cmd, ok := strings.Cut(rest, " cmd=")
	base, err := depFetchBaseDir()
	if !ok || err != nil {
		return false
	}
	return pathWithin(filepath.Join(base, "runs"), dir) && (cmd == command || filepath.Base(cmd) == command)
}
