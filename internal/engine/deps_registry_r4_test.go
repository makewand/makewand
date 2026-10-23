//go:build linux

package engine

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/makewand/makewand/internal/trustedbin"
)

// These tests drive the outage classification through RunVerificationPlan,
// RunRestrictedPlan and EvaluateCandidateFiles, with fake sandboxes, fake
// tools and local servers, so they hold whatever the internals look like.

// unsetProxyEnv keeps proxy settings of the test environment away from the
// connectivity check.
func unsetProxyEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(key, "")
	}
}

// failNamespacesAfter makes every user-mode network launch after the first
// allowed ones unable to open the sandbox namespaces.
func failNamespacesAfter(t *testing.T, allowed ...int) {
	t.Helper()
	old := userNetOpenNamespaces
	launch := 0
	userNetOpenNamespaces = func(ctx context.Context, pid, sandboxPID int) (sandboxNamespaces, error) {
		launch++
		if slices.Contains(allowed, launch) {
			return old(ctx, pid, sandboxPID)
		}
		return sandboxNamespaces{}, errors.New("open the sandbox network namespace: permission denied")
	}
	t.Cleanup(func() { userNetOpenNamespaces = old })
}

// When makewand's own connectivity check cannot get the user-mode network
// that the install had (here its sandbox's namespaces cannot be opened), the
// check is tried once more and then confirms nothing: the failed install is
// the command's failure, not an environment error.
func TestRegistryCheckThatCannotRunConfirmsNothing_R4(t *testing.T) {
	launches := installRunningUserNet(t)
	failNamespacesAfter(t, 1) // the install attaches; both checks cannot
	project, err := NewProject("r4-check-cannot-run", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if err != nil || result == nil || result.ExitCode != 1 {
		t.Fatalf("result = %+v, err = %v, want the failed install as a plain result", result, err)
	}
	lines := strings.Split(strings.TrimSpace(readLog(t, launches)), "\n")
	if len(lines) != 3 || !isFetchDirLaunch(t, lines[0], "npm") {
		t.Fatalf("launches:\n%s\nwant the install, then the check twice", strings.Join(lines, "\n"))
	}
}

// The connectivity check needs no python3: with none, a genuine outage is
// still confirmed and stays an environment error.
func TestRegistryCheckNeedsNoPython_R4(t *testing.T) {
	unsetProxyEnv(t)
	t.Setenv(trustedbin.OverrideEnv("python3"), "/nonexistent/python3")
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := closed.Addr().String()
	_ = closed.Close()
	project := newHostDepsProject(t)
	fakeToolPrinting(t, "go", `go: rsc.io/quote@v1.5.2: Get "https://`+proxyAddr+`/rsc.io/quote/@v/v1.5.2.zip": dial tcp `+proxyAddr+`: connect: connection refused`, 1)
	oldProbe := sandboxGoEnvProbe
	sandboxGoEnvProbe = func(string, string) (hostGoEnv, error) {
		return hostGoEnv{GOPROXY: "https://" + proxyAddr + ",direct", GOSUMDB: "off"}, nil
	}
	t.Cleanup(func() { sandboxGoEnvProbe = oldProbe })
	result, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}})
	if !errors.Is(err, ErrDependencyNetworkUnavailable) || !strings.Contains(err.Error(), "makewand's own connectivity check could not reach 127.0.0.1 (") {
		t.Fatalf("err = %v, want the outage confirmed without python3", err)
	}
	if result == nil || result.ExitCode != 1 {
		t.Fatalf("result = %+v", result)
	}
}

// localGoProxy serves a module proxy over TLS that the check trusts; its
// checksum database support endpoint answers supported. It returns the
// proxy's address and the requests it got.
func localGoProxy(t *testing.T, supported int) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/sumdb/") {
			w.WriteHeader(supported)
		}
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	return strings.TrimPrefix(srv.URL, "https://"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// The go command reaches its checksum database through a module proxy that
// serves it (<proxy>/sumdb/<name>/supported) and connects to the database
// itself only when the proxy does not. makewand's check asks the same way: a
// database that is unreachable directly but served by the proxy is no outage.
func TestGoChecksumDatabaseThroughModuleProxy_R4(t *testing.T) {
	unsetProxyEnv(t)
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sumdb := closed.Addr().String() // the database itself never answers
	_ = closed.Close()
	for _, tc := range []struct {
		name      string
		supported int
		outage    bool
	}{
		{"proxy serves the database", http.StatusOK, false},
		{"proxy does not serve the database", http.StatusNotFound, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, asked := localGoProxy(t, tc.supported)
			project := newHostDepsProject(t)
			fakeToolPrinting(t, "go", `go: rsc.io/quote@v1.5.2: Get "https://`+proxy+`/rsc.io/quote/@v/v1.5.2.zip": dial tcp `+proxy+`: i/o timeout`, 1)
			oldProbe := sandboxGoEnvProbe
			sandboxGoEnvProbe = func(string, string) (hostGoEnv, error) {
				return hostGoEnv{GOPROXY: "https://" + proxy + ",direct", GOSUMDB: sumdb + "+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"}, nil
			}
			t.Cleanup(func() { sandboxGoEnvProbe = oldProbe })
			result, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}})
			if result == nil || result.ExitCode != 1 {
				t.Fatalf("result = %+v", result)
			}
			if got := errors.Is(err, ErrDependencyNetworkUnavailable); got != tc.outage {
				t.Fatalf("outage = %v (err = %v), want %v", got, err, tc.outage)
			}
			if tc.outage && !strings.Contains(err.Error(), "could not reach "+strings.Split(sumdb, ":")[0]+" (") {
				t.Fatalf("err = %v, want the checksum database reported", err)
			}
			if want := "GET /sumdb/" + sumdb + "/supported"; !slices.Contains(asked(), want) {
				t.Fatalf("proxy got %q, want %q", asked(), want)
			}
		})
	}
}

const forgedNpmOutage = `echo 'npm error code ENOTFOUND' >&2
echo 'npm error network request to https://registry.npmjs.org/left-pad failed, reason: getaddrinfo ENOTFOUND registry.npmjs.org' >&2
exit 1`

// A dependency install without outbound network (no slirp4netns here) is an
// environment problem only when it needed a registry: it declares a registry
// dependency that is not installed. Printing a registry outage, as an npm
// lifecycle script can, does not make one.
func TestOfflineOutageNeedsADeclaredRegistryDependency_R4(t *testing.T) {
	for _, tc := range []struct {
		name      string
		manifest  string
		installed bool
		outage    bool
	}{
		{"no registry dependency", `{"name":"x","version":"1.0.0","scripts":{"postinstall":"node forge.js"}}`, false, false},
		{"registry dependency not installed", `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`, false, true},
		{"registry dependency installed", `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`, true, false},
		{"only a URL dependency", `{"name":"x","version":"1.0.0","dependencies":{"y":"https://example.invalid/y-1.0.0.tgz"}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeUserNet(t, fakeUserNetOptions{bwrapTail: forgedNpmOutage, missing: []string{userNetHelperName}})
			project, err := NewProject("r4-offline-npm", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(project.Path, "package.json"), tc.manifest)
			if tc.installed {
				writeFile(t, filepath.Join(project.Path, "node_modules", "left-pad", "package.json"), `{"name":"left-pad","version":"1.3.0"}`)
			}
			result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--no-audit"}})
			if got := errors.Is(err, ErrDependencyNetworkUnavailable); got != tc.outage {
				t.Fatalf("outage = %v (err = %v), want %v", got, err, tc.outage)
			}
			if !tc.outage && (err != nil || result == nil || result.ExitCode != 1) {
				t.Fatalf("result = %+v, err = %v, want the plain failing result", result, err)
			}
		})
	}
}

// fakeCommandBwrap emulates bwrap without the user-mode network protocol: it
// finds the wrapped command and runs pip as a failing install that printed
// pipOutput, pytest as a passing test run and everything else as a success.
func fakeCommandBwrap(pipOutput string) string {
	return fmt.Sprintf(`cmd=
while [ $# -gt 0 ]; do
  case "$1" in
    --setenv|--bind|--ro-bind|--ro-bind-data|--bind-data) shift 3 ;;
    --chdir|--tmpfs|--proc|--dev|--seccomp|--info-fd|--block-fd|--perms|--dir) shift 2 ;;
    --*) shift ;;
    *) cmd=$1; break ;;
  esac
done
case "$cmd" in
pip)
  /bin/cat >&2 <<'MAKEWAND_EOF'
%s
MAKEWAND_EOF
  exit 1 ;;
pytest) echo "1 passed" ;;
esac
exit 0
`, pipOutput)
}

// End to end without slirp4netns: a candidate's in-tree build backend that
// prints pip's own index failure does not make its failed install an
// environment error when the project needs nothing from the index; a project
// whose isolated build needs setuptools from the index does get one.
func TestOfflineBuildBackendForgeryIsCandidateFailure_R4(t *testing.T) {
	for _, tc := range []struct {
		name     string
		requires string
		outage   bool
	}{
		{"no build requirements", `[]`, false},
		{"build requirements from the index", `["setuptools>=61"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeWorkingBwrap(t)
			swapSeccompVars(t, "linux", false, nil)
			oldLookPath := userNetLookPath
			userNetLookPath = func(name string) (string, error) { return "", fmt.Errorf("%w: %s", trustedbin.ErrNotFound, name) }
			t.Cleanup(func() { userNetLookPath = oldLookPath })
			t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
			bwrap := filepath.Join(t.TempDir(), "bwrap")
			writeExecScript(t, bwrap, fakeCommandBwrap(pipBackendForgesIndexFailure))
			verifyLookPath = func(string) (string, error) { return bwrap, nil }
			project, err := NewProject("r4-backend-forge", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			base := []ExtractedFile{
				{Path: "pyproject.toml", Content: "[build-system]\nrequires = " + tc.requires + "\nbuild-backend = \"fbackend\"\nbackend-path = [\".\"]\n\n[project]\nname = \"fvintree\"\nversion = \"0.1.0\"\n\n[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\n"},
				{Path: "fbackend.py", Content: "def build_editable(*args, **kwargs):\n    raise SystemExit(1)\n"},
				{Path: "fvintree.py", Content: "def f():\n    return 1\n"},
				{Path: "tests/test_f.py", Content: "from fvintree import f\n\ndef test_f():\n    assert f() == 1\n"},
			}
			if err := project.WriteFiles(base); err != nil {
				t.Fatal(err)
			}
			if err := project.ScanFiles(); err != nil {
				t.Fatal(err)
			}
			candidate := []ExtractedFile{{Path: "fbackend.py", Content: "import sys\n\ndef get_requires_for_build_editable(config_settings=None):\n    raise SystemExit(1)\n"}}
			report, err := project.EvaluateCandidateFiles(context.Background(), candidate)
			if err != nil {
				t.Fatalf("EvaluateCandidateFiles: %v", err)
			}
			if report.DepsResult == nil || report.DepsResult.ExitCode != 1 || report.Passed {
				t.Fatalf("DepsResult = %+v Passed = %v, want the failed install", report.DepsResult, report.Passed)
			}
			if report.EnvironmentError != tc.outage || strings.Contains(report.DepsError, "could not reach the network") != tc.outage {
				t.Fatalf("EnvironmentError = %v DepsError = %q, want outage %v", report.EnvironmentError, report.DepsError, tc.outage)
			}
		})
	}
}

// Each tool the user-mode network needs is described by what it does: nsenter
// enters the sandbox's namespaces, ip installs the routes.
func TestUserNetToolGuidanceSaysWhatEachToolDoes_R4(t *testing.T) {
	for tool, want := range map[string]string{
		"nsenter": "nsenter, which enters the sandbox's namespaces to attach slirp4netns and configure the sandbox network, was not found",
		"ip":      "ip, which installs the routes that keep the sandbox network off private and host addresses, was not found",
	} {
		t.Run(tool, func(t *testing.T) {
			installFakeUserNet(t, fakeUserNetOptions{missing: []string{tool}})
			launch, network := prepareUserNetwork([]string{"npm", "install"})
			if launch != nil {
				launch.release()
				t.Fatalf("user-network launch prepared without %s", tool)
			}
			if guidance := network.guidance(); !strings.Contains(guidance, want) {
				t.Fatalf("guidance %q lacks %q", guidance, want)
			}
		})
	}
}

// countedSlirp attaches like fakeSlirp on its first launch (the install) and
// fails on every later one (the connectivity check).
func countedSlirp(countFile, logPath string) string {
	return fmt.Sprintf(`count=%q
n=$(/bin/cat "$count" 2>/dev/null || echo 0)
n=$((n+1))
echo $n > "$count"
if [ "$n" -ge 2 ]; then
  echo "slirp4netns: simulated failure on launch $n" >&2
  exit 1
fi
`, countFile) + fakeSlirp(logPath)
}

// When only the connectivity check's own sandbox cannot attach slirp4netns,
// the report says so: the install had outbound network, and the failure
// counts as the candidate's.
func TestCheckAttachFailureReportsTheCheck_R4(t *testing.T) {
	installRunningUserNet(t)
	dir := t.TempDir()
	slirp := filepath.Join(dir, "slirp4netns")
	writeExecScript(t, slirp, countedSlirp(filepath.Join(dir, "count"), filepath.Join(dir, "slirp.log")))
	oldLookPath := userNetLookPath
	userNetLookPath = func(name string) (string, error) {
		if name == userNetHelperName {
			return slirp, nil
		}
		return oldLookPath(name)
	}
	t.Cleanup(func() { userNetLookPath = oldLookPath })
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	project, err := NewProject("r4-check-guidance", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := []ExtractedFile{{Path: "package.json", Content: `{"name":"x","version":"1.0.0","scripts":{"test":"node test.js"},"dependencies":{"left-pad":"1.3.0"}}`}}
	if err := project.WriteFiles(base); err != nil {
		t.Fatal(err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatal(err)
	}
	candidate := []ExtractedFile{{Path: "package.json", Content: `{"name":"x","version":"1.0.1","scripts":{"test":"node test.js"},"dependencies":{"left-pad":"1.3.0"}}`}}
	report, err := project.EvaluateCandidateFiles(context.Background(), candidate)
	if err != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", err)
	}
	if report.EnvironmentError || report.DepsResult == nil || report.DepsResult.ExitCode != 1 {
		t.Fatalf("EnvironmentError = %v DepsResult = %+v DepsError = %q, want the candidate's failed install", report.EnvironmentError, report.DepsResult, report.DepsError)
	}
	for _, want := range []string{"its own connectivity check could not run (slirp4netns could not be attached to its sandbox (", "simulated failure on launch", "the install itself had outbound network through slirp4netns"} {
		if !strings.Contains(report.DepsError, want) {
			t.Fatalf("DepsError = %q, lacks %q", report.DepsError, want)
		}
	}
	if strings.Contains(report.DepsError, "so the dependency install ran without outbound network") {
		t.Fatalf("DepsError = %q, blames the install's network", report.DepsError)
	}
}
