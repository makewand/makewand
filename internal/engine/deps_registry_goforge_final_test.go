//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Final round: without outbound network the go command's own output never
// decides that a failed `go mod tidy` or `go mod download` needed a registry,
// because the go command prints project strings as they are - go.mod tokens
// and import paths - and a candidate can shape them into a report of a failed
// request. go.mod, go.sum, the imports and the module caches decide instead,
// and output that quotes the project's own strings, or the go command's
// report of a go.mod it cannot parse, raises no suspicion at all.

// goForgedText is a report of a failed module proxy request, as a project
// can write it into go.mod or a Go file.
const goForgedText = `Get "https://proxy.golang.org/example.com/x/@v/list": dial tcp: lookup proxy.golang.org: no such host`

// What go 1.27 prints, offline, for each way a project shapes such a report.
const (
	goForgedUnknownDirective = "go: errors parsing go.mod:\ngo.mod:5: unknown directive: `" + goForgedText + "`"
	goForgedReplacement      = "go: example.com/fv imports\n\texample.com/x: example.com/x@v1.0.0: replacement directory ./" + goForgedText + " does not exist"
	goForgedRequirement      = "go: " + goForgedText + "@v1.0.0: module lookup disabled by GOPROXY=off"
	goForgedImport           = "go: example.com/fv imports\n\t" + goForgedText + `: import path "Get \"https://proxy.golang.org/example.com/x/@v/list\": dial tcp: lookup proxy.golang.org: no such host" should not have @version`
	goGenuineProxyFailure    = `go: example.com/dep@v1.2.3: Get "https://proxy.golang.org/example.com/dep/@v/v1.2.3.mod": dial tcp: lookup proxy.golang.org on 127.0.0.53:53: read udp 127.0.0.1:41000->127.0.0.53:53: read: connection refused`
)

// The go command's report of a go.mod (or go.work) it cannot parse names no
// request: it fetched nothing.
func TestGoProxyFailureIgnoresGoModParseErrors_Final(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{goForgedUnknownDirective, false},
		{strings.Replace(goForgedUnknownDirective, "go.mod:", "go.work:", 1), false},
		{strings.Replace(goForgedUnknownDirective, "go.mod:", "sub/go.mod:", 1), false},
		{goGenuineProxyFailure, true},
	} {
		if got := goProxyFailure(&ExecResult{ExitCode: 1, Stderr: tc.stderr}); (got != "") != tc.want {
			t.Errorf("goProxyFailure(%q) = %q, want evidence %v", tc.stderr, got, tc.want)
		}
	}
}

// A report built from the project's own strings - a replacement directory or
// a module path in go.mod, an import path in a Go file - is no evidence of a
// failed request; the same report with no such string in the project is.
func TestGoProxyEvidenceIgnoresProjectStrings_Final(t *testing.T) {
	gomod := "module example.com/fv\n\ngo 1.22\n"
	quoted := strings.ReplaceAll(goForgedText, `"`, `\"`)
	tidy := ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}}
	download := ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "download"}}
	for _, tc := range []struct {
		name   string
		plan   ExecPlan
		files  []string
		stderr string
	}{
		{"replacement directory", tidy, []string{"go.mod", gomod + "\nrequire example.com/x v1.0.0\n\nreplace example.com/x => \"./" + quoted + "\"\n",
			"main.go", "package main\n\nimport _ \"example.com/x\"\n\nfunc main() {}\n"}, goForgedReplacement},
		{"quoted module path", download, []string{"go.mod", gomod + "\nrequire \"" + quoted + "\" v1.0.0\n"}, goForgedRequirement},
		{"raw string import path", tidy, []string{"go.mod", gomod, "main.go", "package main\n\nimport _ `" + goForgedText + "`\n\nfunc main() {}\n"}, goForgedImport},
		{"import path in a replacement module", tidy, []string{"go.mod", gomod, "_vendored/x/x.go", "package x\n\nimport _ \"" + quoted + "\"\n"}, goForgedImport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := &ExecResult{ExitCode: 1, Stderr: tc.stderr}
			if goProxyFailure(result) == "" {
				t.Fatal("the output does not look like a failed request; the case tests nothing")
			}
			if got := newNeedProject(t, tc.files...).registryFailureEvidence(tc.plan, result); got != "" {
				t.Fatalf("evidence = %q, want none: the report is the project's own string", got)
			}
		})
	}
	if got := newNeedProject(t, "go.mod", gomod).registryFailureEvidence(tidy, &ExecResult{ExitCode: 1, Stderr: goGenuineProxyFailure}); got == "" {
		t.Fatal("the go command's own report of a failed request is no longer evidence")
	}
}

// fakeGoParsingOnPath puts a go on PATH that runs the toolchain of the tests
// (so go.mod is parsed by a real go command) and makes env the host Go
// environment.
func fakeGoParsingOnPath(t *testing.T, env hostGoEnv) {
	t.Helper()
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	dir := t.TempDir()
	writeExecScript(t, filepath.Join(dir, "go"), fmt.Sprintf("exec %q \"$@\"\n", realGo))
	t.Setenv("PATH", dir)
	old := sandboxGoEnvProbe
	sandboxGoEnvProbe = func(string) (hostGoEnv, error) { return env, nil }
	t.Cleanup(func() { sandboxGoEnvProbe = old })
}

// cacheGoModule writes module files into a module cache download directory.
func cacheGoModule(t *testing.T, download, escapedPath, version string, exts ...string) {
	t.Helper()
	for _, ext := range exts {
		writeFile(t, filepath.Join(download, filepath.FromSlash(escapedPath), "@v", version+ext), "cached\n")
	}
}

// offlineGoNeed is offlineRegistryNeed for a go plan whose own report of a
// failed request was evidence.
func offlineGoNeed(project *Project, args ...string) string {
	return project.offlineRegistryNeed(ExecPlan{Kind: "deps", Command: "go", Args: args}, "evidence")
}

// Offline, a failed `go mod tidy` or `go mod download` needed the network when
// a module it loads is in neither module cache, when go.sum lacks a
// checksum the checksum database then has to provide, or when an import no
// requirement provides has to be looked up - and not otherwise, whatever it
// printed.
func TestGoOfflineRegistryNeedFromFacts_Final(t *testing.T) {
	const (
		gomod    = "module example.com/fv\n\ngo 1.22\n\nrequire example.com/dep v1.2.3\n"
		mainDep  = "package main\n\nimport _ \"example.com/dep/pkg\"\n\nfunc main() {}\n"
		sums     = "example.com/dep v1.2.3 h1:zip=\nexample.com/dep v1.2.3/go.mod h1:mod=\n"
		missing  = "example.com/dep v1.2.3 (provides example.com/dep/pkg, imported by main.go; not in the sandbox's or the host's module cache)"
		checksum = "example.com/dep v1.2.3 (provides example.com/dep/pkg, imported by main.go; go.sum has no checksum for it, so the go command asks the checksum database sum.golang.org)"
		lookup   = "example.com/new/pkg (imported by main.go; no requirement in go.mod provides it, so go mod tidy looks it up in the module proxy)"
	)
	sandboxCache := ".makewand/sandbox-home/go/pkg/mod/cache/download/"
	for _, tc := range []struct {
		name   string
		env    hostGoEnv
		cached []string // escaped path, version, extensions... in the host cache
		args   []string
		files  []string
		want   string
	}{
		{name: "go.mod the go command cannot parse", args: []string{"mod", "tidy"},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n\n`" + goForgedText + "`\n", "main.go", mainDep}},
		{name: "required module not cached", args: []string{"mod", "tidy"}, files: []string{"go.mod", gomod, "main.go", mainDep}, want: missing},
		{name: "required module cached with checksums", args: []string{"mod", "tidy"}, cached: []string{"example.com/dep", "v1.2.3", ".mod", ".zip"},
			files: []string{"go.mod", gomod, "go.sum", sums, "main.go", mainDep}},
		{name: "required module cached in the sandbox", args: []string{"mod", "tidy"},
			files: []string{"go.mod", gomod, "go.sum", sums, "main.go", mainDep, sandboxCache + "example.com/dep/@v/v1.2.3.mod", "", sandboxCache + "example.com/dep/@v/v1.2.3.zip", ""}},
		{name: "cached without checksums", args: []string{"mod", "tidy"}, cached: []string{"example.com/dep", "v1.2.3", ".mod", ".zip"},
			files: []string{"go.mod", gomod, "main.go", mainDep}, want: checksum},
		{name: "cached without checksums, GONOSUMDB", env: hostGoEnv{GONOSUMDB: "example.com"}, args: []string{"mod", "tidy"},
			cached: []string{"example.com/dep", "v1.2.3", ".mod", ".zip"}, files: []string{"go.mod", gomod, "main.go", mainDep}},
		{name: "cached without checksums, lookup kept in the sandbox", args: []string{"mod", "tidy"}, cached: []string{"example.com/dep", "v1.2.3", ".mod", ".zip"},
			files: []string{"go.mod", gomod, "main.go", mainDep, sandboxCache + "sumdb/sum.golang.org/lookup/example.com/dep@v1.2.3", "x"}},
		{name: "unused requirement, pruned module graph", args: []string{"mod", "tidy"}, files: []string{"go.mod", gomod, "main.go", "package main\n\nfunc main() {}\n"}},
		{name: "unused requirement, unpruned module graph", args: []string{"mod", "tidy"},
			files: []string{"go.mod", strings.Replace(gomod, "go 1.22", "go 1.16", 1), "main.go", "package main\n\nfunc main() {}\n"},
			want:  "example.com/dep v1.2.3 (required in go.mod, whose module graph is not pruned; not in the sandbox's or the host's module cache)"},
		{name: "import no requirement provides", args: []string{"mod", "tidy"},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n", "main.go", "package main\n\nimport _ \"example.com/new/pkg\"\n\nfunc main() {}\n"}, want: lookup},
		{name: "import no requirement provides, GOPROXY=off", env: hostGoEnv{GOPROXY: "off"}, args: []string{"mod", "tidy"},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n", "main.go", "package main\n\nimport _ \"example.com/new/pkg\"\n\nfunc main() {}\n"}},
		{name: "import listed in the host cache, GOPROXY=off", env: hostGoEnv{GOPROXY: "off"}, args: []string{"mod", "tidy"}, cached: []string{"example.com/new", "list", ""},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n", "main.go", "package main\n\nimport _ \"example.com/new/pkg\"\n\nfunc main() {}\n"},
			want:  "example.com/new/pkg (imported by main.go; no requirement in go.mod provides it, and go.sum has no checksum for example.com/new, so the go command asks the checksum database sum.golang.org)"},
		{name: "standard library and cgo imports", args: []string{"mod", "tidy"},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n", "main.go", "package main\n\nimport (\n\t\"C\"\n\t\"fmt\"\n\t\"net/http\"\n)\n\nfunc main() { fmt.Println(http.StatusOK) }\n"}},
		{name: "imports the go command does not load", args: []string{"mod", "tidy"}, files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n",
			"tool.go", "//go:build ignore\n\npackage main\n\nimport _ \"example.com/new/a\"\n",
			"testdata/x.go", "package x\n\nimport _ \"example.com/new/b\"\n",
			"_scratch/x.go", "package x\n\nimport _ \"example.com/new/c\"\n",
			"nested/go.mod", "module example.com/nested\n", "nested/x.go", "package x\n\nimport _ \"example.com/new/d\"\n"}},
		{name: "build constraint that holds", args: []string{"mod", "tidy"}, files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n",
			"main_windows.go", "//go:build windows && !cgo\n\npackage main\n\nimport _ \"example.com/new/pkg\"\n"},
			want: "example.com/new/pkg (imported by main_windows.go; no requirement in go.mod provides it, so go mod tidy looks it up in the module proxy)"},
		{name: "local replacement", args: []string{"mod", "tidy"},
			files: []string{"go.mod", gomod + "\nreplace example.com/dep => ./dep\n", "main.go", mainDep, "dep/go.mod", "module example.com/dep\n"}},
		{name: "replacement module", args: []string{"mod", "tidy"}, files: []string{"go.mod", gomod + "\nreplace example.com/dep => example.com/fork v1.0.0\n", "main.go", mainDep},
			want: "example.com/fork v1.0.0 (provides example.com/dep/pkg, imported by main.go; not in the sandbox's or the host's module cache)"},
		{name: "forged replacement directory", args: []string{"mod", "tidy"},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n\nrequire example.com/x v1.0.0\n\nreplace example.com/x => \"./" + strings.ReplaceAll(goForgedText, `"`, `\"`) + "\"\n",
				"main.go", "package main\n\nimport _ \"example.com/x\"\n\nfunc main() {}\n"}},
		{name: "excluded version", args: []string{"mod", "tidy"}, files: []string{"go.mod", gomod + "\nexclude example.com/dep v1.2.3\n", "main.go", mainDep}},
		{name: "upper case module path cached", args: []string{"mod", "tidy"}, cached: []string{"github.com/!foo/!bar", "v1.0.0", ".mod", ".zip"},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n\nrequire github.com/Foo/Bar v1.0.0\n", "go.sum", "github.com/Foo/Bar v1.0.0 h1:a=\ngithub.com/Foo/Bar v1.0.0/go.mod h1:b=\n",
				"main.go", "package main\n\nimport _ \"github.com/Foo/Bar\"\n\nfunc main() {}\n"}},
		{name: "invalid main module path", args: []string{"mod", "tidy"},
			files: []string{"go.mod", "module \"" + strings.ReplaceAll(goForgedText, `"`, `\"`) + "\"\n\ngo 1.22\n", "main.go", "package main\n\nimport _ \"example.com/new/pkg\"\n\nfunc main() {}\n"}},
		{name: "go.work in the project", args: []string{"mod", "tidy"}, files: []string{"go.mod", gomod, "main.go", mainDep, "go.work", "go 1.22\n\nuse .\n"}},
		{name: "download: unused requirement not cached", args: []string{"mod", "download"}, files: []string{"go.mod", gomod},
			want: "example.com/dep v1.2.3 (required in go.mod; not in the sandbox's or the host's module cache)"},
		{name: "download: only the go.mod file cached", args: []string{"mod", "download", "-x"}, cached: []string{"example.com/dep", "v1.2.3", ".mod"},
			files: []string{"go.mod", gomod, "go.sum", sums}, want: "example.com/dep v1.2.3 (required in go.mod; not in the sandbox's or the host's module cache)"},
		{name: "download: cached with checksums", args: []string{"mod", "download"}, cached: []string{"example.com/dep", "v1.2.3", ".mod", ".zip"}, files: []string{"go.mod", gomod, "go.sum", sums}},
		{name: "download: quoted module path", args: []string{"mod", "download"},
			files: []string{"go.mod", "module example.com/fv\n\ngo 1.22\n\nrequire \"" + strings.ReplaceAll(goForgedText, `"`, `\"`) + "\" v1.0.0\n"}},
		{name: "download of named modules", args: []string{"mod", "download", "example.com/dep@v1.2.3"}, files: []string{"go.mod", gomod}},
		{name: "other go step", args: []string{"build", "./..."}, files: []string{"go.mod", gomod, "main.go", mainDep}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			if len(tc.cached) > 0 {
				env.GOMODCACHE = t.TempDir()
				download := filepath.Join(env.GOMODCACHE, "cache", "download")
				cacheGoModule(t, download, tc.cached[0], tc.cached[1], tc.cached[2:]...)
			}
			fakeGoParsingOnPath(t, env)
			if got := offlineGoNeed(newNeedProject(t, tc.files...), tc.args...); got != tc.want {
				t.Fatalf("need = %q, want %q", got, tc.want)
			}
		})
	}
}

// goSandboxForTest runs verification commands in a real bubblewrap sandbox
// with the real go toolchain and without slirp4netns (loopback only), from a
// HOME and module cache of the test's own. It skips where either is missing.
func goSandboxForTest(t *testing.T) {
	t.Helper()
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The host-side go probes (go env, go mod edit) run with this HOME; turn
	// telemetry off there too so a late counter write cannot race t.TempDir
	// cleanup (the product disables it in the sandbox HOME).
	disableGoTelemetryAt(t, home)
	t.Setenv("PATH", filepath.Dir(realGo)+":/usr/bin:/bin")
	t.Setenv("GOMODCACHE", filepath.Join(t.TempDir(), "modcache"))
	t.Setenv("GOPATH", filepath.Join(t.TempDir(), "gopath"))
	for _, key := range []string{"GOPROXY", "GOSUMDB", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GOFLAGS", "GOENV"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	unsetProxyEnv(t)
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "2G")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	t.Setenv("MAKEWAND_SLIRP4NETNS_PATH", "/nonexistent")
	if _, err := resolveVerifyExecEnvironment(UnsafeHostExecAuthorization{}); err != nil {
		t.Skipf("no working verification sandbox: %v", err)
	}
}

// goForgeProject is a go project whose tests pass, opened for verification.
func goForgeProject(t *testing.T) *Project {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	writeProjectFiles(t, ws,
		"go.mod", "module example.com/fv\n\ngo 1.22\n",
		"main.go", "package main\n\nfunc Hello() string { return \"hi\" }\n\nfunc main() { println(Hello()) }\n",
		"main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n\tif Hello() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n")
	project, err := OpenProject(ws)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// End to end, in a real sandbox without outbound network: candidates whose
// go.mod or Go files make `go mod tidy` print a report of a failed request
// are candidate failures, not environment errors (the verifier's go.mod with
// a backquoted unknown directive among them).
func TestGoTidyForgeriesOfflineAreCandidateFailures_Final(t *testing.T) {
	goSandboxForTest(t)
	quoted := strings.ReplaceAll(goForgedText, `"`, `\"`)
	for _, tc := range []struct {
		name  string
		files []ExtractedFile
	}{
		{"unknown directive in go.mod", []ExtractedFile{
			{Path: "go.mod", Content: "module example.com/fv\n\ngo 1.22\n\n`" + goForgedText + "`\n"},
		}},
		{"replacement directory", []ExtractedFile{
			{Path: "go.mod", Content: "module example.com/fv\n\ngo 1.22\n\nrequire example.com/x v1.0.0\n\nreplace example.com/x => \"./" + quoted + "\"\n"},
			{Path: "main.go", Content: "package main\n\nimport _ \"example.com/x\"\n\nfunc Hello() string { return \"hi\" }\n\nfunc main() { println(Hello()) }\n"},
		}},
		{"raw string import path", []ExtractedFile{
			{Path: "go.mod", Content: "module example.com/fv\n\ngo 1.22\n\n// candidate\n"},
			{Path: "main.go", Content: "package main\n\nimport _ `" + goForgedText + "`\n\nfunc Hello() string { return \"hi\" }\n\nfunc main() { println(Hello()) }\n"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := goForgeProject(t).EvaluateCandidateFiles(context.Background(), tc.files)
			if err != nil {
				t.Fatalf("EvaluateCandidateFiles: %v", err)
			}
			if report.DepsPlan == nil || report.DepsError == "" || report.Passed {
				t.Fatalf("DepsPlan = %+v DepsError = %q Passed = %v, want the failed go mod tidy", report.DepsPlan, report.DepsError, report.Passed)
			}
			if !strings.Contains(report.DepsError, goForgedText) {
				t.Fatalf("DepsError = %q, want go's report quoting the forged text (the case tests nothing otherwise)", report.DepsError)
			}
			if report.EnvironmentError || strings.Contains(report.DepsError, "could not reach the network") || verificationUnattributable(report) {
				t.Fatalf("EnvironmentError = %v DepsError = %q: a forged report made the candidate's failure an environment error", report.EnvironmentError, report.DepsError)
			}
		})
	}
}

// The same for `go mod download` of a go.mod whose quoted module path is the
// forged report.
func TestGoDownloadForgeryOfflineIsCandidateFailure_Final(t *testing.T) {
	goSandboxForTest(t)
	project := goForgeProject(t)
	writeProjectFiles(t, project.Path, "go.mod", "module example.com/fv\n\ngo 1.22\n\nrequire \""+strings.ReplaceAll(goForgedText, `"`, `\"`)+"\" v1.0.0\n")
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "download"}})
	if errors.Is(err, ErrDependencyNetworkUnavailable) {
		t.Fatalf("err = %v: a forged report made the failure an environment error", err)
	}
	if result == nil || result.ExitCode == 0 || !strings.Contains(result.Stderr+result.Stdout, goForgedText) {
		t.Fatalf("result = %+v, err = %v, want go mod download failing with the forged text", result, err)
	}
}

// A genuine need still is an environment error offline: a required module in
// neither module cache, which the go command tries to fetch.
func TestGoGenuineOfflineNeedIsEnvironmentError_Final(t *testing.T) {
	goSandboxForTest(t)
	project := goForgeProject(t)
	writeProjectFiles(t, project.Path,
		"go.mod", "module example.com/fv\n\ngo 1.22\n\nrequire rsc.io/quote/v3 v3.1.0\n",
		"main.go", "package main\n\nimport quote \"rsc.io/quote/v3\"\n\nfunc Hello() string { return quote.HelloV3() }\n\nfunc main() { println(Hello()) }\n")
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}})
	if !errors.Is(err, ErrDependencyNetworkUnavailable) {
		var out string
		if result != nil {
			out = result.Stderr + result.Stdout
		}
		t.Fatalf("err = %v (output %q), want the missing module reported as an environment error", err, out)
	}
}

// The go command reads a Go file through a symlink, so a forged import path
// in the file a symlinked main.go points to counts as the project's string as
// well.
func TestGoProxyEvidenceFollowsSymlinkedGoFiles_Final(t *testing.T) {
	project := newNeedProject(t, "go.mod", "module example.com/fv\n\ngo 1.22\n", "notes.txt", "package main\n\nimport _ `"+goForgedText+"`\n\nfunc main() {}\n")
	if err := os.Symlink("notes.txt", filepath.Join(project.Path, "main.go")); err != nil {
		t.Fatal(err)
	}
	plan := ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}}
	if got := project.registryFailureEvidence(plan, &ExecResult{ExitCode: 1, Stderr: goForgedImport}); got != "" {
		t.Fatalf("evidence = %q, want none: the import path comes from the symlinked file", got)
	}
}
