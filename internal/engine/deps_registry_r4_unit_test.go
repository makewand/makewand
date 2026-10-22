//go:build linux

package engine

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The real probe (makewand's own executable, here the test binary) asks each
// chain in turn while a deciding URL gets 404 or 410, with GET for a URL
// whose answer decides that and HEAD otherwise; a HEAD follows no redirect.
func TestRegistryProbeChains_R4(t *testing.T) {
	unsetProxyEnv(t)
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/served/supported":
			w.WriteHeader(http.StatusOK)
		case "/missing/supported":
			w.WriteHeader(http.StatusNotFound)
		case "/gone/supported":
			w.WriteHeader(http.StatusGone)
		case "/moved":
			http.Redirect(w, r, "https://example.invalid/", http.StatusFound)
		}
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "https://" + closed.Addr().String() + "/"
	_ = closed.Close()
	endpoints := []registryEndpoint{
		{url: srv.URL + "/moved"},
		{url: srv.URL + "/served/supported", decides: true, fallback: &registryEndpoint{url: closedURL}},
		{url: srv.URL + "/missing/supported", decides: true, fallback: &registryEndpoint{url: srv.URL + "/direct"}},
		{url: srv.URL + "/gone/supported", decides: true, fallback: &registryEndpoint{url: closedURL}},
	}
	old := registryCheckEndpoints
	registryCheckEndpoints = func(string) []registryEndpoint { return endpoints }
	t.Cleanup(func() { registryCheckEndpoints = old })

	check := checkRegistryReachable(context.Background(), verifyExecEnvironment{mode: verifyExecUnsafeHost}, "go")
	if !check.done || len(check.unreachable) != 1 || !strings.HasPrefix(check.unreachable[0], "127.0.0.1 (") || !strings.Contains(check.unreachable[0], "connection refused") {
		t.Fatalf("check = %+v, want only the chain ending at the closed port unreachable", check)
	}
	mu.Lock()
	got := slices.Clone(asked)
	mu.Unlock()
	slices.Sort(got)
	want := []string{"GET /gone/supported", "GET /missing/supported", "GET /served/supported", "HEAD /direct", "HEAD /moved"}
	if !slices.Equal(got, want) {
		t.Fatalf("server got %q, want %q", got, want)
	}
}

// The note on a failed install whose connectivity check could not get its
// user-mode network names the check's own failure, after two attempts, and
// that the install itself had outbound network.
func TestRegistryCheckThatCannotRunNote_R4(t *testing.T) {
	installRunningUserNet(t)
	failNamespacesAfter(t, 1)
	_, log := fakeRegistryProbe(t, "reachable")
	project, err := NewProject("r4-check-note", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
	if err != nil || result == nil {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	want := registryOutageNote + "makewand could not confirm it: its own connectivity check could not run (its sandbox's network namespace could not be prepared for slirp4netns (open the sandbox network namespace: permission denied); tried 2 times), while the install itself had outbound network through slirp4netns, so this is reported as a failure of the install itself"
	if result.Note != want {
		t.Fatalf("Note:\n%s\nwant:\n%s", result.Note, want)
	}
	if detail := verificationFailureDetail(ExecPlan{Command: "npm"}, result); !strings.HasSuffix(detail, "\n(makewand: "+want+")") || !strings.HasPrefix(detail, "npm error code ENOTFOUND") {
		t.Fatalf("detail = %q, want the output and then the note", detail)
	}
	if got := readLog(t, log); got != "" {
		t.Fatalf("the probe ran without its network:\n%s", got)
	}
}

// A check whose first attempt cannot get the user-mode network runs again and
// decides on that attempt's answer.
func TestRegistryCheckSecondAttemptDecides_R4(t *testing.T) {
	for _, verdict := range []string{"reachable", "unreachable"} {
		t.Run(verdict, func(t *testing.T) {
			launches := installRunningUserNet(t)
			failNamespacesAfter(t, 1, 3) // the install and the second check attach
			_, log := fakeRegistryProbe(t, verdict)
			project, err := NewProject("r4-check-retry", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
			if outage := errors.Is(err, ErrDependencyNetworkUnavailable); outage != (verdict == "unreachable") {
				t.Fatalf("outage = %v (err = %v)", outage, err)
			}
			if verdict == "reachable" && (result == nil || !strings.Contains(result.Note, "makewand's own connectivity check reached registry.npmjs.org")) {
				t.Fatalf("result = %+v", result)
			}
			if lines := strings.Split(strings.TrimSpace(readLog(t, launches)), "\n"); len(lines) != 3 {
				t.Fatalf("launches:\n%s\nwant the install and two checks", strings.Join(lines, "\n"))
			}
			if _, urls := registryCheckLog(t, log); !slices.Equal(urls, []string{"https://registry.npmjs.org/"}) {
				t.Fatalf("probe asked %q, want one run", urls)
			}
		})
	}
}

// dependencyRegistryError: what each kind of network makes of output that
// suggests an outage, and the note that explains an unconfirmed one.
func TestDependencyRegistryErrorNotes_R4(t *testing.T) {
	failed := &ExecResult{ExitCode: 1, Stderr: npmRegistryENOTFOUND}
	deps := ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}}
	userMode := sandboxNetwork{kind: sandboxNetworkUserMode}
	host := sandboxNetwork{kind: sandboxNetworkHost}
	offline := sandboxNetwork{kind: sandboxNetworkNone, detail: "slirp4netns was not found"}
	for _, tc := range []struct {
		name     string
		check    registryCheck
		network  sandboxNetwork
		manifest string
		outage   bool
		note     string
	}{
		{"reached", registryCheck{done: true, reached: []string{"registry.npmjs.org"}}, userMode, "", false, "makewand's own connectivity check reached registry.npmjs.org, so this is reported as a failure of the install itself (the project may name a registry, index or mirror that does not answer)"},
		{"unreachable", registryCheck{done: true, unreachable: []string{"registry.npmjs.org (no response)"}}, userMode, "", true, ""},
		{"check cannot run, user mode", registryCheck{failure: "boom; tried 2 times"}, userMode, "", false, "makewand could not confirm it: its own connectivity check could not run (boom; tried 2 times), while the install itself had outbound network through slirp4netns"},
		{"check cannot run, host", registryCheck{failure: "boom"}, host, "", false, "while the install itself ran with the host network"},
		{"offline, nothing needed", registryCheck{}, offline, `{"name":"x","version":"1.0.0"}`, false, "the install ran without outbound network and makewand found nothing it had to download from a package registry"},
		{"offline, dependency needed", registryCheck{}, offline, `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked := stubRegistryCheck(t, tc.check)
			project, err := NewProject("r4-notes", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if tc.manifest != "" {
				writeFile(t, filepath.Join(project.Path, "package.json"), tc.manifest)
			}
			note, err := project.dependencyRegistryError(context.Background(), verifyExecEnvironment{}, deps, failed, tc.network)
			if got := errors.Is(err, ErrDependencyNetworkUnavailable); got != tc.outage {
				t.Fatalf("outage = %v (err = %v), want %v", got, err, tc.outage)
			}
			if tc.note == "" && note != "" || tc.note != "" && (!strings.HasPrefix(note, registryOutageNote) || !strings.Contains(note, tc.note)) {
				t.Fatalf("note = %q, want %q", note, tc.note)
			}
			if ran := len(*checked) > 0; ran != (tc.network.kind != sandboxNetworkNone) {
				t.Fatalf("connectivity check ran = %v for network %+v", ran, tc.network)
			}
		})
	}
	// A verification that is being canceled gets no note.
	stubRegistryCheck(t, registryCheck{})
	project, err := NewProject("r4-notes-canceled", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if note, err := project.dependencyRegistryError(ctx, verifyExecEnvironment{}, deps, failed, userMode); note != "" || err != nil {
		t.Fatalf("canceled: note = %q, err = %v", note, err)
	}
}

// Guidance for a step without outbound network, and the connectivity check's
// own failures, which never blame the install's network.
func TestSandboxNetworkGuidanceAndCheckFailures_R4(t *testing.T) {
	if got := (sandboxNetwork{kind: sandboxNetworkNone, detail: "slirp4netns was not found"}).guidance(); !strings.HasPrefix(got, "dependency installs run without outbound network because slirp4netns was not found; install slirp4netns") {
		t.Fatalf("guidance = %q", got)
	}
	if got := (sandboxNetwork{kind: sandboxNetworkNone, detail: "nsenter was not found"}).checkFailure(); got != "its sandbox would have had no outbound network: nsenter was not found" {
		t.Fatalf("checkFailure = %q", got)
	}
	for _, tc := range []struct {
		failure userNetFailure
		want    string
	}{
		{userNetFailure{stage: userNetStageHelper, cause: "EOF: slirp4netns: boom"}, "slirp4netns could not be attached to its sandbox (EOF: slirp4netns: boom)"},
		{userNetFailure{stage: userNetStageRoutes, cause: "ip: exit status 2"}, "the routes that keep its sandbox off private and host addresses could not be installed (ip: exit status 2)"},
		{userNetFailure{stage: userNetStageRoutes, cause: "ip: exit status 1: nsenter: cannot open"}, "nsenter could not enter its sandbox's namespaces (ip: exit status 1: nsenter: cannot open)"},
		{userNetFailure{stage: userNetStageSandbox, cause: "sandbox did not report its namespace: EOF"}, "its sandbox's network namespace could not be prepared for slirp4netns (sandbox did not report its namespace: EOF)"},
	} {
		got := tc.failure.checkFailure()
		if got != tc.want || strings.Contains(got, "ran without outbound network") {
			t.Errorf("%+v: %q, want %q", tc.failure, got, tc.want)
		}
		if via := (sandboxNetwork{kind: sandboxNetworkNone, attach: &tc.failure}).checkFailure(); via != tc.want {
			t.Errorf("through the network: %q", via)
		}
	}
}

// fakeGoOnPath puts a go command (never run: the go environment is stubbed)
// on PATH and stubs the host go environment.
func fakeGoOnPath(t *testing.T, env hostGoEnv) {
	t.Helper()
	dir := t.TempDir()
	writeExecScript(t, filepath.Join(dir, "go"), "exit 1\n")
	t.Setenv("PATH", dir)
	old := sandboxGoEnvProbe
	sandboxGoEnvProbe = func(string) (hostGoEnv, error) { return env, nil }
	t.Cleanup(func() { sandboxGoEnvProbe = old })
}

// The endpoints of a go install: the module proxy, and the checksum database
// as the go command reaches it - through each proxy's support endpoint, then
// directly - unless GOSUMDB names its URL or turns it off.
func TestGoRegistryEndpoints_R4(t *testing.T) {
	cacheWithSumDB := t.TempDir()
	writeFile(t, filepath.Join(cacheWithSumDB, "cache", "download", "sumdb", "sum.golang.org", "supported"), "")
	cacheWithout := t.TempDir()
	writeFile(t, filepath.Join(cacheWithout, "cache", "download", "golang.org", "x", "text", "@v", "list"), "")
	const key = "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"
	for _, tc := range []struct {
		name string
		env  hostGoEnv
		want [][]string
	}{
		{"defaults", hostGoEnv{}, [][]string{
			{"https://proxy.golang.org"},
			{"https://proxy.golang.org/sumdb/sum.golang.org/supported", "https://sum.golang.org/"},
		}},
		{"mirror proxy", hostGoEnv{GOPROXY: "https://goproxy.cn,direct", GOSUMDB: "sum.golang.org"}, [][]string{
			{"https://goproxy.cn"},
			{"https://goproxy.cn/sumdb/sum.golang.org/supported", "https://sum.golang.org/"},
		}},
		{"two proxies", hostGoEnv{GOPROXY: "https://a.example/,https://b.example|direct", GOSUMDB: key}, [][]string{
			{"https://a.example/"},
			{"https://a.example/sumdb/sum.golang.org/supported", "https://b.example/sumdb/sum.golang.org/supported", "https://sum.golang.org/"},
		}},
		{"alias for mainland China", hostGoEnv{GOPROXY: "https://goproxy.cn,direct", GOSUMDB: "sum.golang.google.cn"}, [][]string{
			{"https://goproxy.cn"},
			{"https://sum.golang.google.cn"},
		}},
		{"URL in GOSUMDB, no proxy", hostGoEnv{GOPROXY: "direct", GOSUMDB: key + " https://sum.example/"}, [][]string{
			{"https://sum.example/"},
		}},
		{"proxy off", hostGoEnv{GOPROXY: "off"}, [][]string{
			{"https://sum.golang.org/"},
		}},
		{"checksum database off, credentials dropped", hostGoEnv{GOPROXY: "file:///cache,https://user:secret@private.example,https://mirror.example|direct", GOSUMDB: "off"}, [][]string{
			{"https://mirror.example"},
		}},
		{"GOSUMDB with credentials ignored", hostGoEnv{GOPROXY: "https://goproxy.cn", GOSUMDB: key + " https://user:pw@sum.example/"}, [][]string{
			{"https://goproxy.cn"},
			{"https://goproxy.cn/sumdb/sum.golang.org/supported", "https://sum.golang.org/"},
		}},
		{"GOPRIVATE covers every module", hostGoEnv{GOPROXY: "https://goproxy.cn", GOPRIVATE: "*"}, [][]string{
			{"https://goproxy.cn"},
		}},
		{"GONOSUMDB covers every module", hostGoEnv{GONOSUMDB: "corp.example,*"}, [][]string{
			{"https://proxy.golang.org"},
		}},
		{"GONOSUMDB covers some modules", hostGoEnv{GONOSUMDB: "corp.example", GOPRIVATE: "*"}, [][]string{
			{"https://proxy.golang.org"},
			{"https://proxy.golang.org/sumdb/sum.golang.org/supported", "https://sum.golang.org/"},
		}},
		{"module cache serves the database", hostGoEnv{GOMODCACHE: cacheWithSumDB}, [][]string{
			{"https://proxy.golang.org"},
		}},
		{"module cache without the database", hostGoEnv{GOMODCACHE: cacheWithout}, [][]string{
			{"https://proxy.golang.org"},
			{"https://proxy.golang.org/sumdb/sum.golang.org/supported", "https://sum.golang.org/"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeGoOnPath(t, tc.env)
			var got [][]string
			for _, endpoint := range defaultRegistryEndpoints("go") {
				got = append(got, endpoint.chain())
			}
			if !slices.EqualFunc(got, tc.want, slices.Equal[[]string]) {
				t.Fatalf("chains = %q, want %q", got, tc.want)
			}
		})
	}
	for command, want := range map[string]string{"npm": "https://registry.npmjs.org/", "pip3": "https://pypi.org/simple/", "cargo": "https://index.crates.io/config.json"} {
		if got := defaultRegistryEndpoints(command); len(got) == 0 || got[0].url != want || got[0].fallback != nil {
			t.Errorf("%s: %+v, want %q first", command, got, want)
		}
	}
}

// parseRegistryCheck follows a chain: for a deciding URL, 404 or 410 hands
// over to the fallback, 200 decides and any other answer leaves the service
// unavailable (as the go command treats it); a fallback the probe did not
// need to ask needs no line.
func TestParseRegistryCheckChains_R4(t *testing.T) {
	endpoint := []registryEndpoint{{url: "https://proxy.example/sumdb/s/supported", decides: true, fallback: &registryEndpoint{url: "https://s.example/"}}}
	for _, tc := range []struct {
		stdout      string
		done        bool
		unreachable []string
		reached     []string
	}{
		{"reachable https://proxy.example/sumdb/s/supported 200\n", true, nil, []string{"proxy.example"}},
		{"reachable https://proxy.example/sumdb/s/supported 500\n", true, []string{"proxy.example (answered 500 Internal Server Error)"}, nil},
		{"reachable https://proxy.example/sumdb/s/supported 404\nreachable https://s.example/ 403\n", true, nil, []string{"s.example"}},
		{"reachable https://proxy.example/sumdb/s/supported 410\nunreachable https://s.example/ no route\n", true, []string{"s.example (no route)"}, nil},
		{"unreachable https://proxy.example/sumdb/s/supported refused\n", true, []string{"proxy.example (refused)"}, nil},
		{"reachable https://proxy.example/sumdb/s/supported 404\n", false, nil, nil},
	} {
		check := parseRegistryCheck(registryProbeRun{stdout: tc.stdout}, endpoint, false)
		if check.done != tc.done || !slices.Equal(check.unreachable, tc.unreachable) || !slices.Equal(check.reached, tc.reached) {
			t.Errorf("%q: %+v", tc.stdout, check)
		}
	}
}

// writeProjectFiles writes files (path, content pairs) into dir.
func writeProjectFiles(t *testing.T, dir string, files ...string) {
	t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(files[i])), files[i+1])
	}
}

func newNeedProject(t *testing.T, files ...string) *Project {
	t.Helper()
	isolateFetchArea(t)
	project, err := NewProject("r4-need", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFiles(t, project.Path, files...)
	return project
}

func TestNpmRegistryNeed_R4(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  string
	}{
		{"dependency", []string{"package.json", `{"dependencies":{"left-pad":"^1.3.0"}}`}, "left-pad (dependencies in package.json)"},
		{"installed", []string{"package.json", `{"dependencies":{"left-pad":"1.3.0"}}`, "node_modules/left-pad/package.json", `{}`}, ""},
		{"scoped", []string{"package.json", `{"dependencies":{"@scope/pkg":"latest"}}`}, "@scope/pkg (dependencies in package.json)"},
		{"alias", []string{"package.json", `{"dependencies":{"pad":"npm:left-pad@1.3.0"}}`}, "pad (dependencies in package.json)"},
		{"dev dependency", []string{"package.json", `{"devDependencies":{"jest":"29"}}`}, "jest (devDependencies in package.json)"},
		{"dev dependency the .npmrc may omit", []string{"package.json", `{"devDependencies":{"jest":"29"}}`, ".npmrc", "omit=dev\n"}, ""},
		{"not from the registry", []string{"package.json", `{"dependencies":{"a":"file:../a","b":"git+https://example.invalid/b.git","c":"user/repo","d":"https://example.invalid/d.tgz","e":"workspace:*","f":"link:../f","g":"./g"}}`}, ""},
		{"optional and peer only", []string{"package.json", `{"optionalDependencies":{"fsevents":"2"},"peerDependencies":{"react":"18"}}`}, ""},
		{"invalid name", []string{"package.json", `{"dependencies":{"../evil":"1.0.0"}}`}, ""},
		{"invalid manifest", []string{"package.json", `{"dependencies":`}, ""},
		{"no manifest", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newNeedProject(t, tc.files...).offlineRegistryNeed(ExecPlan{Command: "npm", Args: []string{"install"}}, "evidence"); got != tc.want {
				t.Fatalf("need = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPipRegistryNeed_R4(t *testing.T) {
	const evidence = "pip's own report"
	inTree := "[build-system]\nrequires = []\nbuild-backend = \"fbackend\"\nbackend-path = [\".\"]\n\n[project]\nname = \"x\"\nversion = \"0.1.0\"\n"
	editable := []string{"install", "--user", "-e", ".", "--use-pep517"}
	requirements := []string{"install", "--user", "-r", "requirements.txt"}
	userSite := ".makewand/sandbox-home/.local/lib/python3.99/site-packages/"
	for _, tc := range []struct {
		name  string
		args  []string
		files []string
		want  string
	}{
		{"named requirements only: pip's own report", requirements, []string{"requirements.txt", "six==1.16.0\n"}, evidence},
		{"wheel only: pip's own report", []string{"install", "--user", "dist/x-1.0-py3-none-any.whl"}, nil, evidence},
		{"in-tree backend, nothing to download", editable, []string{"pyproject.toml", inTree}, ""},
		{"build requirements", editable, []string{"pyproject.toml", "[build-system]\nrequires = [\n  \"setuptools>=61\", # comment\n  \"wheel\",\n]\n"}, "setuptools>=61 (build requirement for pyproject.toml)"},
		{"default build backend", editable, []string{"pyproject.toml", "[project]\nname = \"x\"\n"}, "setuptools>=40.8.0 (build requirement for pyproject.toml)"},
		{"build requirements with markers only", editable, []string{"pyproject.toml", "[build-system]\nrequires = [\"tomli; python_version < '3.11'\"]\n"}, ""},
		{"no build isolation", append(slices.Clone(editable), "--no-build-isolation"), []string{"pyproject.toml", "[build-system]\nrequires = [\"setuptools>=61\"]\n"}, ""},
		{"static dependency not installed", editable, []string{"pyproject.toml", inTree + "dependencies = [\"makewand-r4-absent==1.0\", \"other; sys_platform == 'win32'\"]\n"}, "makewand-r4-absent==1.0 (dependencies in pyproject.toml)"},
		{"static dependency installed", editable, []string{"pyproject.toml", inTree + "dependencies = [\"Makewand_R4.Present>=1\"]\n", userSite + "makewand_r4_present-1.0.dist-info/METADATA", ""}, ""},
		{"dynamic dependencies", editable, []string{"pyproject.toml", inTree + "dynamic = [\"dependencies\"]\ndependencies = [\"makewand-r4-absent\"]\n"}, ""},
		{"setup.py without PEP 517", []string{"install", "--user", "-e", "."}, []string{"setup.py", "from setuptools import setup\nsetup()\n"}, ""},
		{"setup.py under PEP 517", editable, []string{"setup.py", "from setuptools import setup\nsetup()\n"}, "setuptools>=40.8.0 (build requirement for setup.py)"},
		{"local project in requirements", requirements, []string{"requirements.txt", "-e .\nmakewand-r4-absent==2.0 --hash=sha256:00\n", "pyproject.toml", inTree}, "makewand-r4-absent==2.0 (requirements.txt)"},
		{"nested requirements resolve from their file", requirements, []string{"requirements.txt", "-r reqs/base.txt\n.\n", "reqs/base.txt", "makewand-r4-absent \\\n  >=1.0\n# makewand-r4-commented\n", "pyproject.toml", inTree}, "makewand-r4-absent >=1.0 (reqs/base.txt)"},
		{"no index", requirements, []string{"requirements.txt", "--no-index\n-e .\nmakewand-r4-absent\n", "pyproject.toml", inTree}, ""},
		{"find links", requirements, []string{"requirements.txt", "-f ./wheels\n-e .\nmakewand-r4-absent\n", "pyproject.toml", inTree}, ""},
		{"local index", []string{"install", "-i", "file:///srv/index", "-e", "."}, []string{"pyproject.toml", inTree + "dependencies = [\"makewand-r4-absent\"]\n"}, ""},
		{"pip configuration", editable, []string{"pyproject.toml", "[build-system]\nrequires = [\"setuptools\"]\n", ".makewand/sandbox-home/.config/pip/pip.conf", "[install]\nno-build-isolation = true\n"}, ""},
		{"requirements makewand cannot read", []string{"install", "-r", "../outside.txt"}, nil, ""},
		{"requirements that include themselves", requirements, []string{"requirements.txt", strings.Repeat("-r requirements.txt\n", 50) + "-e .\nmakewand-r4-absent\n", "pyproject.toml", inTree}, "makewand-r4-absent (requirements.txt)"},
		{"source archive", []string{"install", "./dist/x-1.0.tar.gz"}, nil, ""},
		{"direct reference to a local file", []string{"install", "x @ file:///srv/x"}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := newNeedProject(t, tc.files...)
			if got := project.offlineRegistryNeed(ExecPlan{Command: "pip", Args: tc.args}, evidence); got != tc.want {
				t.Fatalf("need = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCargoRegistryNeed_R4(t *testing.T) {
	const manifest = "[package]\nname = \"fv\"\nversion = \"0.1.0\"\n\n[dependencies]\nitoa = \"1\"\n"
	const lock = "version = 4\n\n[[package]]\nname = \"fv\"\nversion = \"0.1.0\"\ndependencies = [\n \"itoa\",\n]\n\n[[package]]\nname = \"itoa\"\nversion = \"1.0.18\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\nchecksum = \"x\"\n"
	cached := ".makewand/sandbox-home/.cargo/registry/cache/index.crates.io-1949cf8c6b5b557f/"
	build := []string{"build"}
	fetch := []string{"fetch"}
	for _, tc := range []struct {
		name  string
		args  []string
		files []string
		want  string
	}{
		{"no lock", build, []string{"Cargo.toml", manifest}, "itoa (Cargo.toml; without a Cargo.lock cargo resolves it from the registry index)"},
		{"no lock, path dependency only", build, []string{"Cargo.toml", "[package]\nname = \"fv\"\n[dependencies]\nlib = { path = \"lib\" }\n"}, ""},
		{"locked crate not cached", build, []string{"Cargo.toml", manifest, "Cargo.lock", lock}, "itoa 1.0.18 (Cargo.lock; not in the sandbox's cargo cache)"},
		{"locked crate cached", build, []string{"Cargo.toml", manifest, "Cargo.lock", lock, cached + "itoa-1.0.18.crate", "x"}, ""},
		{"stale lock", build, []string{"Cargo.toml", manifest + "ryu = { version = \"1\", features = [\"small\"] }\n", "Cargo.lock", lock, cached + "itoa-1.0.18.crate", "x"}, "ryu (Cargo.toml, not in Cargo.lock; cargo resolves it from the registry index)"},
		{"dev-dependency the build does not need", build, []string{"Cargo.toml", "[package]\nname = \"fv\"\n\n[dev-dependencies]\nitoa = \"1\"\n", "Cargo.lock", lock}, ""},
		{"dev-dependency fetch needs", fetch, []string{"Cargo.toml", "[package]\nname = \"fv\"\n\n[dev-dependencies]\nitoa = \"1\"\n", "Cargo.lock", lock}, "itoa 1.0.18 (Cargo.lock; not in the sandbox's cargo cache)"},
		{"through a path dependency", build, []string{"Cargo.toml", "[package]\nname = \"fv\"\n\n[dependencies.lib]\npath = \"lib\"\n", "Cargo.lock", "[[package]]\nname = \"fv\"\nversion = \"0.1.0\"\ndependencies = [\"lib\"]\n\n[[package]]\nname = \"lib\"\nversion = \"0.1.0\"\ndependencies = [\"itoa 1.0.18\"]\n\n[[package]]\nname = \"itoa\"\nversion = \"1.0.18\"\nsource = \"sparse+https://index.crates.io/\"\n"}, "itoa 1.0.18 (Cargo.lock; not in the sandbox's cargo cache)"},
		{"renamed dependency", build, []string{"Cargo.toml", "[package]\nname = \"fv\"\n[dependencies]\nnum = { package = \"itoa\", version = \"1\" }\n", "Cargo.lock", lock}, "itoa 1.0.18 (Cargo.lock; not in the sandbox's cargo cache)"},
		{"workspace member inherits", build, []string{"Cargo.toml", "[workspace]\nmembers = [\n  \"crates/*\",\n]\n\n[workspace.dependencies]\nitoa = \"1\"\n", "crates/a/Cargo.toml", "[package]\nname = \"a\"\n\n[dependencies]\nitoa.workspace = true\n"}, "itoa (Cargo.toml; without a Cargo.lock cargo resolves it from the registry index)"},
		{"renamed workspace dependency", build, []string{"Cargo.toml", "[workspace]\nmembers = [\"a\"]\n\n[workspace.dependencies]\nnum = { package = \"itoa\", version = \"1\" }\n", "a/Cargo.toml", "[package]\nname = \"a\"\n\n[dependencies]\nnum = { workspace = true }\n", "Cargo.lock", lock}, "itoa 1.0.18 (Cargo.lock; not in the sandbox's cargo cache)"},
		{"git dependency", build, []string{"Cargo.toml", "[package]\nname = \"fv\"\n[dependencies]\nx = { git = \"https://example.invalid/x\" }\n"}, ""},
		{"offline configuration", build, []string{"Cargo.toml", manifest, ".cargo/config.toml", "[net]\noffline = true\n"}, ""},
		{"vendored sources", build, []string{"Cargo.toml", manifest, ".cargo/config.toml", "[source.crates-io]\nreplace-with = \"vendored\"\n\n[source.vendored]\ndirectory = \"vendor\"\n"}, ""},
		{"remote mirror", build, []string{"Cargo.toml", manifest, ".cargo/config.toml", "[source.crates-io]\nreplace-with = \"mirror\"\n\n[source.mirror]\nregistry = \"sparse+http://10.255.255.1/index/\"\n"}, "itoa (Cargo.toml; without a Cargo.lock cargo resolves it from the registry index)"},
		{"sandbox HOME configuration", build, []string{"Cargo.toml", manifest, ".makewand/sandbox-home/.cargo/config.toml", "net.offline = true\n"}, ""},
		{"patch section", build, []string{"Cargo.toml", manifest + "\n[patch.crates-io]\nitoa = { path = \"itoa\" }\n"}, ""},
		{"offline flag", []string{"build", "--offline"}, []string{"Cargo.toml", manifest}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := newNeedProject(t, tc.files...)
			if got := project.offlineRegistryNeed(ExecPlan{Command: "cargo", Args: tc.args}, "evidence"); got != tc.want {
				t.Fatalf("need = %q, want %q", got, tc.want)
			}
		})
	}
}

// go mod tidy and go mod download run no project code but print project
// strings as they are, so the go command's own report never decides: a
// project that requires and imports nothing needed no registry, whatever the
// report says (TestGoOfflineRegistryNeedFromFacts_Final covers the facts that
// do). Any other go step gets no environment verdict offline.
func TestGoOfflineRegistryNeed_R4(t *testing.T) {
	project := newNeedProject(t, "go.mod", "module example.com/x\n")
	for args, want := range map[string]string{"mod tidy": "", "mod download": "", "build ./...": "", "generate ./...": ""} {
		if got := project.offlineRegistryNeed(ExecPlan{Command: "go", Args: strings.Fields(args)}, "evidence"); got != want {
			t.Errorf("go %s: %q, want %q", args, got, want)
		}
	}
}

func TestParseTOML_R4(t *testing.T) {
	doc := parseTOML(`# comment
name = "top" # trailing
[ build-system ]
requires = [
  "setuptools>=61", # a comment with "quotes" and ]
  'wheel',
]
build-backend = "a#b=c"

[target.'cfg(target_os = "linux")'.dependencies]
libc = { version = "0.2", features = ["std", "extra_traits"], default-features = false }

[[package]]
name = "a"
[[package]]
name = "b"
dependencies = ["a 1.0 (registry+https://x)"]
`)
	if v, ok := doc.value("build-system", "requires"); !ok || !slices.Equal(tomlStrings(v), []string{"setuptools>=61", "wheel"}) {
		t.Fatalf("requires = %q, %v", v, ok)
	}
	if v, _ := doc.value("build-system", "build-backend"); firstTOMLString(v) != "a#b=c" {
		t.Fatalf("build-backend = %q", v)
	}
	if !doc.hasTable("build-system") || doc.hasTable("project") {
		t.Fatalf("tables = %v", doc.tables)
	}
	libc := ""
	var packages []string
	for _, entry := range doc.entries {
		switch {
		case entry.key == "libc":
			libc = entry.table
			fields := tomlInlineTable(entry.value)
			if firstTOMLString(fields["version"]) != "0.2" || !slices.Equal(tomlStrings(fields["features"]), []string{"std", "extra_traits"}) || fields["default-features"] != "false" {
				t.Fatalf("inline table = %q", fields)
			}
		case entry.table == "package" && entry.key == "name":
			packages = append(packages, firstTOMLString(entry.value)+"@"+strconv.Itoa(entry.index))
		}
	}
	if libc != `target.'cfg(target_os = "linux")'.dependencies` {
		t.Fatalf("libc table = %q", libc)
	}
	if !slices.Equal(packages, []string{"a@1", "b@2"}) {
		t.Fatalf("packages = %q", packages)
	}
	if got := cargoLockPackages(`[[package]]
name = "b"
version = "1.0.0"
source = "registry+https://x"
dependencies = [
 "a 1.0 (registry+https://x)",
 "c",
]
`); len(got) != 1 || !got[0].fromRegistry() || !slices.Equal(got[0].dependencies, []string{"a 1.0 (registry+https://x)", "c"}) {
		t.Fatalf("lock packages = %+v", got)
	}
}
