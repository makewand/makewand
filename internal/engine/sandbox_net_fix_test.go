//go:build linux

package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makewand/makewand/internal/seccompbpf"
	"github.com/makewand/makewand/internal/trustedbin"
)

func writeFixScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
}

func readFixFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// fakeNpmOnPath makes a fake npm the only tool on PATH, so host-mode
// verification (the acknowledged unsafe opt-in) runs it instead of a real npm.
func fakeNpmOnPath(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	writeFixScript(t, filepath.Join(dir, "npm"), body)
	t.Setenv("PATH", dir)
}

// newNpmVerificationProject creates an npm project whose baseline declares a
// registry dependency and a real test script.
func newNpmVerificationProject(t *testing.T, extra ...ExtractedFile) *Project {
	t.Helper()
	allowHostExecForTest(t)
	project, err := NewProject("npm-fix", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	project.SetUnsafeHostExecAuthorization(UnsafeHostExecAuthorization{Acknowledged: true, Source: "test"})
	files := append([]ExtractedFile{
		{Path: "package.json", Content: `{"name":"x","version":"1.0.0","scripts":{"test":"node test.js"},"dependencies":{"left-pad":"1.3.0"}}`},
		{Path: "index.js", Content: "module.exports = 1;\n"},
		{Path: "test.js", Content: "require('./index.js');\n"},
	}, extra...)
	if err := project.WriteFiles(files); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}
	return project
}

const fakeNpmRegistryUnreachable = `case "$1" in
install)
  echo "npm error code ENOTFOUND" >&2
  echo "npm error network request to https://registry.npmjs.org/left-pad failed, reason: getaddrinfo ENOTFOUND registry.npmjs.org" >&2
  exit 1 ;;
*) echo "ok"; exit 0 ;;
esac
`

// A dependency install that cannot reach its registry says nothing about the
// candidate: it is an environment error, not a rejected (quality-penalised)
// candidate.
func TestEvaluateCandidateFilesDependencyNetworkFailureIsEnvironmentError_FixN1(t *testing.T) {
	stubRegistryCheck(t, unreachableRegistry)
	project := newNpmVerificationProject(t)
	fakeNpmOnPath(t, fakeNpmRegistryUnreachable)
	files := []ExtractedFile{{Path: "index.js", Content: "module.exports = 2;\n"}}

	report, err := project.EvaluateCandidateFiles(context.Background(), files)
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.Passed || !report.EnvironmentError {
		t.Fatalf("Passed=%v EnvironmentError=%v DepsError=%q, want an environment error", report.Passed, report.EnvironmentError, report.DepsError)
	}
	if !strings.Contains(report.DepsError, "could not reach the network") || !strings.Contains(report.DepsError, "not a candidate failure") {
		t.Fatalf("DepsError = %q, want network guidance", report.DepsError)
	}
	if report.DepsResult == nil || report.DepsResult.ExitCode != 1 {
		t.Fatalf("DepsResult = %+v, want the failed install result", report.DepsResult)
	}
	if report.TestsResult != nil {
		t.Fatalf("tests ran after the environment failure: %+v", report.TestsResult)
	}
	attempt := CandidateAttempt{Provider: "claude", Requested: "claude", Files: files, Content: RenderExtractedFiles(files), Verification: report}
	if ShouldRecordCandidateQuality(attempt) {
		t.Fatal("a registry outage must not be recorded against the provider")
	}
	if stage := CandidateAttemptStage(context.Background(), attempt); stage != CandidateProgressUnverified {
		t.Fatalf("stage = %v, want unverified (not rejected)", stage)
	}
}

// A dependency install that fails for a reason the candidate controls (here
// an invalid version spec) stays a candidate failure.
func TestEvaluateCandidateFilesDependencyFailureWithoutNetworkErrorStaysCandidateFailure_FixN1(t *testing.T) {
	project := newNpmVerificationProject(t)
	fakeNpmOnPath(t, `case "$1" in
install)
  echo "npm error code EINVALIDTAGNAME" >&2
  echo "npm error Invalid tag name \"not a version\" of package \"left-pad@not a version\"" >&2
  exit 1 ;;
*) exit 0 ;;
esac
`)
	files := []ExtractedFile{{Path: "index.js", Content: "module.exports = 2;\n"}}

	report, err := project.EvaluateCandidateFiles(context.Background(), files)
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.Passed || report.EnvironmentError || !strings.Contains(report.DepsError, "EINVALIDTAGNAME") {
		t.Fatalf("Passed=%v EnvironmentError=%v DepsError=%q, want a candidate failure", report.Passed, report.EnvironmentError, report.DepsError)
	}
	attempt := CandidateAttempt{Provider: "claude", Requested: "claude", Files: files, Content: RenderExtractedFiles(files), Verification: report}
	if stage := CandidateAttemptStage(context.Background(), attempt); stage != CandidateProgressRejected {
		t.Fatalf("stage = %v, want rejected", stage)
	}
}

// The build flow (RunRestrictedPlan) stops instead of auto-fixing when a
// command returns an error; a registry outage must therefore come back as an
// error, while failing tests keep their plain non-zero result.
func TestRunRestrictedPlanDependencyNetworkFailureReturnsError_FixN1(t *testing.T) {
	stubRegistryCheck(t, unreachableRegistry)
	project := newNpmVerificationProject(t)
	// npm always prints the error code line before the failed request.
	fakeNpmOnPath(t, `echo "npm error code EAI_AGAIN" >&2
echo "npm error request to https://registry.npmjs.org/x failed, reason: getaddrinfo EAI_AGAIN registry.npmjs.org" >&2
exit 1
`)
	deps := ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}}
	result, err := project.RunRestrictedPlan(context.Background(), deps)
	if !errors.Is(err, ErrDependencyNetworkUnavailable) {
		t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable", err)
	}
	if result == nil || result.ExitCode != 1 {
		t.Fatalf("result = %+v, want the failed install result alongside the error", result)
	}

	tests := ExecPlan{Kind: "tests", Command: "npm", Args: []string{"test"}}
	result, err = project.RunRestrictedPlan(context.Background(), tests)
	if err != nil || result == nil || result.ExitCode != 1 {
		t.Fatalf("tests: result=%+v err=%v, want a plain failing result", result, err)
	}
}

// The production classifier (dependencyRegistryError): the package manager's
// own outage diagnostics raise the suspicion, makewand's connectivity check
// (stubbed here as unreachable) confirms it, and the guidance matches the
// network the install had.
func TestDependencyNetworkErrorClassification_FixN1(t *testing.T) {
	checked := stubRegistryCheck(t, registryCheck{done: true, unreachable: []string{"registry.example (no response)"}})
	project, err := NewProject("classify-fix", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}}
	userMode := sandboxNetwork{kind: sandboxNetworkUserMode}
	cases := []struct {
		name   string
		plan   ExecPlan
		result *ExecResult
		want   bool
	}{
		{"npm ENOTFOUND", deps, &ExecResult{ExitCode: 1, Stderr: npmRegistryENOTFOUND}, true},
		{"pip index", ExecPlan{Kind: "deps", Command: "pip"}, &ExecResult{ExitCode: 1, Stderr: pipIndexUnreachable}, true},
		{"go proxy", ExecPlan{Kind: "deps", Command: "go"}, &ExecResult{ExitCode: 1, Stderr: `go: example.com/m imports x: Get "https://proxy.golang.org/x/@v/list": dial tcp: lookup proxy.golang.org: no such host`}, true},
		{"cargo index", ExecPlan{Kind: "deps", Command: "cargo"}, &ExecResult{ExitCode: 101, Stderr: cargoIndexUnreachable}, true},
		{"npm 404 is the candidate's", deps, &ExecResult{ExitCode: 1, Stderr: "npm error code E404\nnpm error 404 Not Found - GET https://registry.npmjs.org/nonexistent-pkg"}, false},
		{"successful install with retried request", deps, &ExecResult{ExitCode: 0, Stderr: "npm warn retry getaddrinfo EAI_AGAIN"}, false},
		{"tests are never reclassified", ExecPlan{Kind: "tests", Command: "npm"}, &ExecResult{ExitCode: 1, Stderr: "Error: connect ECONNREFUSED 127.0.0.1:5432"}, false},
		{"no result", deps, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := project.dependencyRegistryError(context.Background(), verifyExecEnvironment{}, tc.plan, tc.result, userMode)
			if got := errors.Is(err, ErrDependencyNetworkUnavailable); got != tc.want {
				t.Fatalf("classified = %v (err=%v), want %v", got, err, tc.want)
			}
		})
	}
	*checked = nil
	failed := &ExecResult{ExitCode: 1, Stderr: npmRegistryENOTFOUND}
	// The loopback-only step needs the registry: it declares a dependency
	// that is not installed.
	writeFile(t, filepath.Join(project.Path, "package.json"), `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`)
	for _, network := range []sandboxNetwork{{kind: sandboxNetworkNone, detail: "slirp4netns was not found"}, userMode, {kind: sandboxNetworkHost}} {
		_, err := project.dependencyRegistryError(context.Background(), verifyExecEnvironment{}, deps, failed, network)
		if err == nil || !strings.Contains(err.Error(), "not a candidate failure") {
			t.Fatalf("network %+v: err = %v, want environment guidance", network, err)
		}
		if network.kind == sandboxNetworkNone && !strings.Contains(err.Error(), "slirp4netns was not found") {
			t.Fatalf("missing helper detail in %v", err)
		}
		if network.kind != sandboxNetworkNone && !strings.Contains(err.Error(), "makewand's own connectivity check could not reach registry.example (no response)") {
			t.Fatalf("network %+v: err = %v, want the connectivity check's result", network, err)
		}
	}
	// A step without outbound network reaches no registry: nothing to check.
	if want := []string{"npm", "npm"}; !slices.Equal(*checked, want) {
		t.Fatalf("connectivity checks = %q, want %q (none for the loopback-only step)", *checked, want)
	}
}

// Verification must not create package-lock.json when the workspace has none:
// npm's generated lockfile is a new input file that fails the integrity check
// and gets blamed on the candidate.
func TestVerificationNpmInstallWithoutLockfileWritesNoLockfile_FixN1(t *testing.T) {
	for _, withLock := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-lockfile", true: "with-lockfile"}[withLock], func(t *testing.T) {
			var extra []ExtractedFile
			if withLock {
				extra = append(extra, ExtractedFile{Path: "package-lock.json", Content: `{"lockfileVersion":3}` + "\n"})
			}
			project := newNpmVerificationProject(t, extra...)
			argLog := filepath.Join(t.TempDir(), "npm-args.log")
			// Mimics npm: install writes package-lock.json unless told not to.
			fakeNpmOnPath(t, fmt.Sprintf(`echo "$*" >> %q
case "$1" in
install)
  case " $* " in
  *" --no-package-lock "*) ;;
  *) [ -f package-lock.json ] || echo '{"lockfileVersion":3,"generated":true}' > package-lock.json ;;
  esac
  exit 0 ;;
test) echo "1 passing"; exit 0 ;;
esac
`, argLog))

			report, err := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{Path: "index.js", Content: "module.exports = 2;\n"}})
			if err != nil {
				t.Fatalf("EvaluateCandidateFiles: %v", err)
			}
			if !report.Passed || report.IntegrityError != "" {
				t.Fatalf("Passed=%v IntegrityError=%q DepsError=%q TestsError=%q, want a pass", report.Passed, report.IntegrityError, report.DepsError, report.TestsError)
			}
			args := readFixFile(t, argLog)
			flagged := strings.Contains(args, "install --ignore-scripts --no-package-lock")
			if flagged == withLock || !strings.Contains(args, "install --ignore-scripts") {
				t.Fatalf("npm invocations:\n%s\nwant --no-package-lock only without a committed lockfile", args)
			}
			if report.DepsPlan == nil || slicesContainString(report.DepsPlan.Args, "--no-package-lock") {
				t.Fatalf("reported DepsPlan = %+v, want the baseline plan", report.DepsPlan)
			}
		})
	}
}

// declareNpmRegistryDependency gives the project in dir a package.json with a
// registry dependency that is not installed: an install without outbound
// network that fails then needed the registry (offlineRegistryNeed).
func declareNpmRegistryDependency(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`)
}

func slicesContainString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// fakeUserNetBwrap emulates the bwrap side of the user-mode network protocol.
// A user-network launch reports its own PID on --info-fd as the sandbox
// child, with that child's /proc entries under proc; like bwrap's child it
// writes the uid map only after reporting, reads the --ro-bind-data resolver
// file (still in its first user namespace), and then waits on --block-fd,
// where end-of-file releases it just as data does. Then it "runs" the
// command. Every event carries the bwrap PID.
// The uid-map log follows the write and can lag behind makewand observing it;
// resolver consumption can run concurrently with network configuration. The
// optional schedules force both orderings without relying on scheduler luck.
func fakeUserNetBwrap(logPath, proc string, opts fakeUserNetOptions, tail string) string {
	uidMap := opts.uidMap
	if uidMap == "" {
		uidMap = fmt.Sprintf("0 %d 1", os.Geteuid())
	}
	sharedNet := map[bool]string{false: "0", true: "1"}[opts.sharedNet]
	return fmt.Sprintf(`log=%q
proc=%q
uidmap=%q
ppid=%q
sharednet=%s
schedule=%q
info=; block=; rfd=; rdst=
while [ $# -gt 0 ]; do
  case "$1" in
    --info-fd) info=$2; shift 2 ;;
    --block-fd) block=$2; shift 2 ;;
    --ro-bind-data) rfd=$2; rdst=$3; shift 3 ;;
    --setenv) echo "bwrap[$$]: setenv $2=$3" >> "$log"; shift 3 ;;
    *) shift ;;
  esac
done
if [ -n "$info" ]; then
  child="$proc/$$"
  /bin/mkdir -p "$child/ns"
  echo user > "$child/ns/user"
  if [ "$sharednet" = 1 ]; then /bin/ln "$proc/self/ns/net" "$child/ns/net"; else echo net > "$child/ns/net"; fi
  echo "$$ (bwrap) S ${ppid:-$$} 0 0" > "$child/stat"
  : > "$child/uid_map"
  eval "printf '{\"child-pid\": %%d}\n' $$ >&$info"
  eval "exec $info>&-"
  echo "bwrap[$$]: info-sent" >> "$log"
  (
    if [ "$schedule" = resolver-after-network ]; then
      while [ ! -f "$log.nft-done" ]; do /bin/sleep 0.001; done
    fi
    eval "/bin/cat <&$rfd" > "$log.resolv"
    printf '%%s' "$rdst" > "$log.resolvdst"
    echo "bwrap[$$]: resolv-read" >> "$log"
  ) >/dev/null 2>&1 &
  /bin/sleep 0.2
  if [ "$uidmap" != "-" ]; then
    printf '%%s\n' "$uidmap" > "$child/uid_map"
    if [ "$schedule" = uid-map-log-after-resolver ]; then wait; fi
    echo "bwrap[$$]: uid-map-written" >> "$log"
  fi
  wait
  eval "read -r unblocked <&$block"
  echo "bwrap[$$]: released" >> "$log"
else
  echo "bwrap[$$]: no-info-fd" >> "$log"
fi
echo "bwrap[$$]: ran" >> "$log"
echo "sandboxed command ran"
%s
`, logPath, proc, uidMap, opts.childPPID, sharedNet, opts.schedule, tail)
}

// fakeSlirp emulates slirp4netns: it logs its argv and the namespace it was
// handed, reports readiness on --ready-fd and exits when --exit-fd is closed.
// It runs with an empty environment, hence the absolute paths.
// Log readiness before signaling it, so configuration events cannot overtake
// the log entry after makewand receives the readiness byte.
func fakeSlirp(logPath string) string {
	return fmt.Sprintf(`log=%q
echo "slirp: args $*" >> "$log"
for a in "$@"; do
  case "$a" in
    --ready-fd=*) ready=${a#--ready-fd=} ;;
    --exit-fd=*) exitfd=${a#--exit-fd=} ;;
    /proc/self/fd/*) echo "slirp: netns $(/bin/readlink "$a")" >> "$log" ;;
  esac
done
echo "slirp: ready-sent" >> "$log"
eval "printf 1 >&$ready"
eval "/bin/cat <&$exitfd >/dev/null"
echo "slirp: exit-fd closed" >> "$log"
`, logPath)
}

// fakeNsenter logs the namespaces it was asked to enter (resolving the
// descriptor paths) and runs the command.
func fakeNsenter(logPath string) string {
	return fmt.Sprintf(`log=%q
ns=
while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; break ;;
    --user=*|--net=*) ns="$ns ${1%%%%=*}=$(/bin/readlink "${1#*=}")" ;;
    *) ns="$ns $1" ;;
  esac
  shift
done
echo "nsenter:$ns -- $*" >> "$log"
exec "$@"
`, logPath)
}

// fakeNetConfigTool logs its argv and stdin (to log.<name>) and exits with
// status after printing output. Successful configuration is logged after all
// input has been consumed, before the tool exits.
func fakeNetConfigTool(logPath, name, output string, status int) string {
	return fmt.Sprintf(`log=%q
echo "%s: $*" >> "$log"
/bin/cat > "$log.%s"
[ -z %q ] || echo %q >&2
if [ %d -eq 0 ]; then
  echo "%s: configured" >> "$log"
  : > "$log.%s-done"
fi
exit %d
`, logPath, name, name, output, output, status, name, name, status)
}

type fakeUserNetOptions struct {
	seccomp   bool
	bwrapTail string // shell run after the command "ran"
	schedule  string // force a late uid-map log or resolver read, using barriers rather than timing
	slirpBody string // replaces the fake slirp4netns
	uidMap    string // the child's uid map ("" maps root to the test user, "-" never writes one)
	childPPID string // the parent PID in the child's stat ("" is the bwrap process)
	sharedNet bool   // the child is in makewand's network namespace
	ipOutput  string
	ipExit    int
	nftExit   int
	missing   []string // tools the trusted lookup does not find
}

type fakeUserNetEnv struct {
	dir    string
	log    string
	resolv string
	proc   string
	tools  map[string]string
}

func installFakeUserNet(t *testing.T, opts fakeUserNetOptions) fakeUserNetEnv {
	t.Helper()
	fakeWorkingBwrap(t)
	swapSeccompVars(t, "linux", opts.seccomp, nil)
	dir := t.TempDir()
	env := fakeUserNetEnv{
		dir:    dir,
		log:    filepath.Join(dir, "events.log"),
		resolv: filepath.Join(dir, "resolv.conf"),
		proc:   filepath.Join(dir, "proc"),
		tools:  map[string]string{},
	}
	writeFile(t, env.resolv, "nameserver 127.0.0.53\n")
	writeFile(t, filepath.Join(env.proc, "self", "ns", "user"), "makewand user\n")
	writeFile(t, filepath.Join(env.proc, "self", "ns", "net"), "makewand net\n")
	bwrap := filepath.Join(dir, "bwrap")
	writeFixScript(t, bwrap, fakeUserNetBwrap(env.log, env.proc, opts, opts.bwrapTail))
	verifyLookPath = func(string) (string, error) { return bwrap, nil }
	slirpBody := opts.slirpBody
	if slirpBody == "" {
		slirpBody = fakeSlirp(env.log)
	}
	for name, body := range map[string]string{
		userNetHelperName: slirpBody,
		"nsenter":         fakeNsenter(env.log),
		"ip":              fakeNetConfigTool(env.log, "ip", opts.ipOutput, opts.ipExit),
		"nft":             fakeNetConfigTool(env.log, "nft", "", opts.nftExit),
	} {
		env.tools[name] = filepath.Join(dir, "bin", name)
		writeFixScript(t, env.tools[name], body)
	}
	oldLookPath, oldResolv, oldProc, oldAddrs := userNetLookPath, userNetResolvConf, userNetProcDir, userNetHostAddrs
	userNetLookPath = func(name string) (string, error) {
		for _, missing := range opts.missing {
			if name == missing {
				return "", fmt.Errorf("%w: %s", trustedbin.ErrNotFound, name)
			}
		}
		if path, ok := env.tools[name]; ok {
			return path, nil
		}
		return "", fmt.Errorf("%w: %s", trustedbin.ErrNotFound, name)
	}
	userNetResolvConf = env.resolv
	userNetProcDir = env.proc
	userNetHostAddrs = func() ([]net.Addr, error) { return nil, nil }
	t.Cleanup(func() {
		userNetLookPath, userNetResolvConf, userNetProcDir, userNetHostAddrs = oldLookPath, oldResolv, oldProc, oldAddrs
	})
	return env
}

// fakeUserNetConfiguration lets a test whose fake bwrap speaks only the
// --info-fd/--block-fd protocol attach its fake slirp4netns: the sandbox
// namespaces are stand-in files, nsenter runs its command and ip and nft
// succeed. Call it after installing the fake userNetLookPath.
func fakeUserNetConfiguration(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "config.log")
	tools := map[string]string{}
	for name, body := range map[string]string{
		"nsenter": fakeNsenter(log),
		"ip":      fakeNetConfigTool(log, "ip", "", 0),
		"nft":     fakeNetConfigTool(log, "nft", "", 0),
	} {
		tools[name] = filepath.Join(dir, name)
		writeFixScript(t, tools[name], body)
	}
	for _, name := range []string{"user", "net"} {
		writeFile(t, filepath.Join(dir, "ns", name), name+"\n")
	}
	oldLookPath, oldOpen := userNetLookPath, userNetOpenNamespaces
	userNetLookPath = func(name string) (string, error) {
		if path, ok := tools[name]; ok {
			return path, nil
		}
		return oldLookPath(name)
	}
	userNetOpenNamespaces = func(context.Context, int, int) (sandboxNamespaces, error) {
		user, err := os.Open(filepath.Join(dir, "ns", "user"))
		if err != nil {
			return sandboxNamespaces{}, err
		}
		netns, err := os.Open(filepath.Join(dir, "ns", "net"))
		if err != nil {
			_ = user.Close()
			return sandboxNamespaces{}, err
		}
		return sandboxNamespaces{user: user, net: netns}, nil
	}
	t.Cleanup(func() { userNetLookPath, userNetOpenNamespaces = oldLookPath, oldOpen })
}

// userNetLaunchPID returns the PID of the fake bwrap that ran a user-network
// launch (the one that reported a child), or "".
func userNetLaunchPID(events []string) string {
	for _, event := range events {
		if rest, ok := strings.CutPrefix(event, "bwrap["); ok {
			if pid, tail, ok := strings.Cut(rest, "]: "); ok && tail == "info-sent" {
				return pid
			}
		}
	}
	return ""
}

func eventLogged(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

func eventLines(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(readFixFile(t, path)), "\n")
}

// assertEventOrder checks that each prefix appears in order in the event log.
func assertEventOrder(t *testing.T, events []string, prefixes ...string) {
	t.Helper()
	next := 0
	for _, event := range events {
		if next < len(prefixes) && strings.HasPrefix(event, prefixes[next]) {
			next++
		}
	}
	if next != len(prefixes) {
		t.Fatalf("events out of order or missing %q:\n%s", prefixes[next:], strings.Join(events, "\n"))
	}
}

// The dependency install sandbox is held until slirp4netns has configured the
// sandbox network namespace and the blocking routes are installed; the helper
// is told to exit once the command finishes. The namespaces are those of the
// child bwrap reports, entered through nsenter once its uid map is written,
// and the child descriptors follow the seccomp descriptor when the filter is
// in use.
func TestDependencyInstallAttachesUserModeNetwork_FixN1(t *testing.T) {
	for _, seccomp := range []bool{false, true} {
		for _, schedule := range []string{"normal", "uid-map-log-after-resolver", "resolver-after-network"} {
			// Subtest names avoid "=": env(1) in the memory-scope wrapper would
			// misread a temp path containing it as an assignment.
			t.Run(map[bool]string{false: "seccomp-off", true: "seccomp-on"}[seccomp]+"/"+schedule, func(t *testing.T) {
				if seccomp && !seccompbpf.Supported() {
					t.Skip("no seccomp filter for this architecture")
				}
				env := installFakeUserNet(t, fakeUserNetOptions{seccomp: seccomp, bwrapTail: "exit 0", schedule: schedule})
				project, err := NewProject("usernet-fake", t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}})
				if err != nil || result == nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "sandboxed command ran") {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				events := eventLines(t, env.log)
				pid := userNetLaunchPID(events)
				child := filepath.Join(env.proc, pid, "ns")
				bw := "bwrap[" + pid + "]: "
				// The uid_map write precedes namespace validation, but its log can
				// lag behind resolver consumption or helper startup. DNS setup and
				// network setup may overlap; both must precede command execution.
				assertEventOrder(t, events, bw+"info-sent", bw+"uid-map-written", bw+"released")
				assertEventOrder(t, events, bw+"info-sent", bw+"resolv-read", bw+"released", bw+"ran")
				assertEventOrder(t, events,
					"nsenter: --user="+filepath.Join(child, "user")+" --preserve-credentials -- "+env.tools[userNetHelperName]+" ",
					"slirp: args", "slirp: ready-sent",
					"nsenter: --user="+filepath.Join(child, "user")+" --preserve-credentials --net="+filepath.Join(child, "net")+" -- "+env.tools["ip"]+" -4 -batch -",
					"ip: -4 -batch -", "ip: configured", "nft: -f -", "nft: configured",
					bw+"released", bw+"ran", "slirp: exit-fd closed")
				switch schedule {
				case "uid-map-log-after-resolver":
					assertEventOrder(t, events, bw+"resolv-read", bw+"uid-map-written")
				case "resolver-after-network":
					assertEventOrder(t, events, "nft: configured", bw+"resolv-read")
				}
				var slirpArgs string
				for _, event := range events {
					if rest, ok := strings.CutPrefix(event, "slirp: args "); ok {
						slirpArgs = rest
					}
				}
				for _, want := range []string{"--configure", "--disable-host-loopback", "--enable-sandbox", "--enable-seccomp", "--ready-fd=5", "--exit-fd=6", "--netns-type=path", "/proc/self/fd/4 " + userNetDevice} {
					if !strings.Contains(" "+slirpArgs+" ", " "+want+" ") {
						t.Fatalf("slirp4netns args %q lack %q", slirpArgs, want)
					}
				}
				if !eventLogged(events, "slirp: netns "+filepath.Join(child, "net")) {
					t.Fatalf("slirp4netns was not handed the sandbox network namespace:\n%s", strings.Join(events, "\n"))
				}
				if got, want := readFixFile(t, env.log+".ip"), userNetRouteScript(userNetBlockedPrefixes(nil)); got != want {
					t.Fatalf("ip batch:\n%s\nwant:\n%s", got, want)
				}
				if got, want := readFixFile(t, env.log+".nft"), userNetFilterRuleset(userNetBlockedPrefixes(nil)); got != want {
					t.Fatalf("nft ruleset:\n%s\nwant:\n%s", got, want)
				}
				if got := readFixFile(t, env.log+".resolv"); got != "nameserver "+userNetDNS+"\n" {
					t.Fatalf("sandbox resolv.conf = %q", got)
				}
				if got := readFixFile(t, env.log+".resolvdst"); got != env.resolv {
					t.Fatalf("resolv.conf bound at %q, want %q", got, env.resolv)
				}
				if strings.Contains(strings.Join(events, "\n"), "npm_config_fetch_retries") {
					t.Fatalf("offline npm settings applied although the network was attached:\n%s", strings.Join(events, "\n"))
				}
			})
		}
	}
}

func TestNonDependencyStepsGetNoUserModeNetwork_FixN1(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{bwrapTail: "exit 0"})
	project, err := NewProject("usernet-tests", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"tests", "quickcheck"} {
		if _, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: kind, Command: "npm", Args: []string{"test"}}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	log := readFixFile(t, env.log)
	if strings.Contains(log, "slirp:") || strings.Contains(log, "nsenter:") || strings.Contains(log, "info-sent") || !strings.Contains(log, "]: no-info-fd") {
		t.Fatalf("non-dependency steps must stay loopback-only:\n%s", log)
	}
}

// When slirp4netns cannot attach, the held command never runs with that
// launch, and a registry failure of the step is an environment error naming
// the attach failure.
func TestDependencyInstallHelperFailureNeverRunsHeldCommand_FixN1(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{
		bwrapTail: "echo \"npm error code ENOTFOUND\" >&2\nexit 1",
		slirpBody: "echo 'slirp4netns: cannot open /dev/net/tun' >&2\nexit 1\n",
	})
	project, err := NewProject("usernet-helper-fail", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	declareNpmRegistryDependency(t, project.Path)
	start := time.Now()
	_, err = project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if !errors.Is(err, ErrDependencyNetworkUnavailable) || !strings.Contains(err.Error(), "/dev/net/tun") {
		t.Fatalf("err = %v, want an environment error naming the attach failure", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("attach failure took %s", elapsed)
	}
	events := eventLines(t, env.log)
	pid := userNetLaunchPID(events)
	if pid == "" || eventLogged(events, "bwrap["+pid+"]: ran") || eventLogged(events, "bwrap["+pid+"]: released") || strings.Contains(strings.Join(events, "\n"), "ip: ") {
		t.Fatalf("the held command ran or was released after the attach failure:\n%s", strings.Join(events, "\n"))
	}
}

// Without a trusted slirp4netns the install runs loopback-only with npm
// retries disabled, and a registry failure explains how to enable networking.
func TestDependencyInstallWithoutHelperFailsFastWithGuidance_FixN1(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{
		bwrapTail: "echo \"npm error code EAI_AGAIN\" >&2\nexit 1",
		missing:   []string{userNetHelperName},
	})
	project, err := NewProject("usernet-missing", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	declareNpmRegistryDependency(t, project.Path)
	_, err = project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if !errors.Is(err, ErrDependencyNetworkUnavailable) || !strings.Contains(err.Error(), "slirp4netns was not found in a trusted system directory") {
		t.Fatalf("err = %v, want missing-helper guidance", err)
	}
	log := readFixFile(t, env.log)
	if !strings.Contains(log, "]: setenv npm_config_fetch_retries=0") || !strings.Contains(log, "]: no-info-fd") {
		t.Fatalf("expected loopback-only install with npm retries disabled:\n%s", log)
	}
}

// A sandbox that dies before reporting its namespace must not hang the launch
// or start the helper; the failure stays a sandbox (environment) error.
func TestDependencyInstallSandboxSetupFailureDoesNotStartHelper_FixN1(t *testing.T) {
	env := installFakeUserNet(t, fakeUserNetOptions{})
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	writeFixScript(t, bwrap, "echo 'bwrap: setting up uid map: Permission denied' >&2\nexit 1\n")
	verifyLookPath = func(string) (string, error) { return bwrap, nil }
	project, err := NewProject("usernet-bwrap-fail", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if err == nil || !strings.Contains(err.Error(), "verification sandbox failed to start") || !isEnvironmentExecError(err) {
		t.Fatalf("err = %v, want a sandbox start (environment) error", err)
	}
	if strings.Contains(readFixFile(t, env.log), "slirp:") {
		t.Fatal("helper started although the sandbox never reported its namespace")
	}
}

func TestSandboxResolvConfTarget_FixN1(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "resolv.conf")
	writeFile(t, regular, "nameserver 192.0.2.1\n")
	if got := sandboxResolvConfTarget(regular); got != regular {
		t.Fatalf("regular file target = %q, want %q", got, regular)
	}
	// systemd-resolved: the link points into /run, which the sandbox replaces
	// with a tmpfs, so the data must be bound at the link target.
	stub := "/run/makewand-fix-test-" + rand.Text() + "/stub-resolv.conf"
	link := filepath.Join(dir, "resolv-link.conf")
	if err := os.Symlink(stub, link); err != nil {
		t.Fatal(err)
	}
	if got := sandboxResolvConfTarget(link); got != stub {
		t.Fatalf("symlink target = %q, want %q", got, stub)
	}
	// A missing file outside the sandbox tmpfs mounts: bwrap could not
	// create it on the read-only root.
	if got := sandboxResolvConfTarget("/nonexistent-makewand-fix-" + rand.Text() + "/resolv.conf"); got != "" {
		t.Fatalf("missing file target = %q, want empty", got)
	}
}

// hostNonLoopbackIPv4 returns an address of this host that a sandbox could
// reach only through the user-mode network, or "" when there is none.
func hostNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil && !ipNet.IP.IsLinkLocalUnicast() {
			return ipNet.IP.String()
		}
	}
	return ""
}

// Live check of the security goal: a dependency install gets the user-mode
// network, yet host loopback services (directly and through the slirp
// gateway), a service on the host's own address and host abstract AF_UNIX
// sockets stay unreachable. Other destinations are covered by
// TestDependencyInstallNetworkBlocksNonPublicDestinations_DepsNet.
func TestDependencyInstallUserNetworkHidesHostLoopback_FixN1(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	if _, err := trustedbin.LookPath(userNetHelperName); err != nil {
		t.Skipf("slirp4netns unavailable: %v", err)
	}
	loopback, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Close()
	abstractName := "makewand-fix-n1-" + rand.Text()
	abstract, err := net.Listen("unix", "@"+abstractName)
	if err != nil {
		t.Fatal(err)
	}
	defer abstract.Close()
	hostService := ""
	if ip := hostNonLoopbackIPv4(); ip != "" {
		if l, err := net.Listen("tcp4", net.JoinHostPort(ip, "0")); err == nil {
			defer l.Close()
			hostService = l.Addr().String()
		}
	}

	project, err := NewProject("usernet-live", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := `import socket, sys
loop_port, abstract, host_service = int(sys.argv[1]), sys.argv[2], sys.argv[3]
def reachable(host, port):
    s = socket.socket()
    s.settimeout(3)
    try:
        return s.connect_ex((host, port)) == 0
    finally:
        s.close()
with open("/proc/net/route") as f:
    assert any(line.split()[1] == "00000000" for line in list(f)[1:]), "no default route: user-mode network not attached"
with open("/etc/resolv.conf") as f:
    assert "nameserver 10.0.2.3" in f.read(), "resolver not pointed at the slirp4netns forwarder"
assert not reachable("127.0.0.1", loop_port), "host loopback reachable via 127.0.0.1"
assert not reachable("10.0.2.2", loop_port), "host loopback reachable via the slirp4netns gateway"
u = socket.socket(socket.AF_UNIX)
try:
    u.connect("\0" + abstract)
except OSError:
    pass
else:
    raise SystemExit("host abstract socket reachable")
if host_service:
    host, port = host_service.rsplit(":", 1)
    assert not reachable(host, int(port)), "a service on the host's own address reachable through the user-mode network"
print("ok")
`
	port := strconv.Itoa(loopback.Addr().(*net.TCPAddr).Port)
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "python3", Args: []string{"-c", script, port, abstractName, hostService}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("dependency sandbox network: err=%v result=%+v", err, result)
	}
}
