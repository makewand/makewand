//go:build linux

package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// GEC-1021-1 round 7: units of the manifest-only fetch (PEP 440, markers,
// wheel tags, index pages, pip configuration, the go and cargo fetch sources,
// node_modules placement) and live end-to-end fetches with the real tools.

func TestPEP440Specifiers_GEC1021R7(t *testing.T) {
	for _, tc := range []struct {
		spec, version string
		want          bool
	}{
		{"==999999", "2.32.3", false},
		{"==2.32.3", "2.32.3", true},
		{"==2.32", "2.32.0", true},
		{"==1.4.*", "1.4.7", true},
		{"==1.4.*", "1.5", false},
		{"!=1.4.*", "1.5", true},
		{">=1,<2", "1.9", true},
		{">=1,<2", "2.0", false},
		{"<2", "2.0rc1", false},
		{"<2rc2", "2.0rc1", true},
		{">1.0", "1.0.post1", false},
		{">1.0.post1", "1.0.post2", true},
		{">1.0", "1.0+local", false},
		{"<=1.0", "1.0+local", true},
		{"~=1.4.2", "1.4.9", true},
		{"~=1.4.2", "1.5.0", false},
		{"~=2.2", "2.9", true},
		{"~=2.2", "3.0", false},
		{"===foo", "foo", true},
		{">=1", "1!0.5", true},
		{"", "3.0", true},
		{">=1.0", "2.0a1", false}, // a pre-release only when asked for
		{">=2.0a0", "2.0a1", true},
		{"==1.0", "1.0.0", true},
		{"==1.0+abc", "1.0", false},
		{"==1.0", "1.0+abc", true},
	} {
		specs, ok := parsePEP440Specifiers(tc.spec)
		if !ok {
			t.Errorf("parsePEP440Specifiers(%q) failed", tc.spec)
			continue
		}
		if got := pep440Contains(specs, tc.version, false); got != tc.want {
			t.Errorf("%q contains %q = %v, want %v", tc.spec, tc.version, got, tc.want)
		}
	}
	for _, bad := range []string{"~=1", ">=not a version", "=>1", "==1,,"} {
		if _, ok := parsePEP440Specifiers(bad); ok {
			t.Errorf("parsePEP440Specifiers(%q) accepted", bad)
		}
	}
	for _, pair := range [][2]string{{"1.0.dev1", "1.0a1"}, {"1.0a1", "1.0b1"}, {"1.0rc1", "1.0"}, {"1.0", "1.0.post1"}, {"1.0", "1.0+a"}, {"1.0+a", "1.0+1"}, {"1.0.post1.dev1", "1.0.post1"}} {
		a, _ := parsePEP440(pair[0])
		b, _ := parsePEP440(pair[1])
		if cmpPEP440(a, b) >= 0 {
			t.Errorf("%s should sort before %s", pair[0], pair[1])
		}
	}
}

func TestPipMarkerEvaluate_GEC1021R7(t *testing.T) {
	env := pipMarkerEnv{"python_version": "3.12", "python_full_version": "3.12.3", "sys_platform": "linux", "os_name": "posix",
		"platform_machine": "x86_64", "implementation_name": "cpython", "platform_python_implementation": "CPython", "platform_release": "7.0.0-1013-nvidia"}
	for _, tc := range []struct {
		marker      string
		holds, know bool
	}{
		{`python_version < "2"`, false, true},
		{`python_version >= "3.8"`, true, true},
		{`python_version > "3"`, true, true},
		{`"3.8" <= python_version`, true, true},
		{`sys_platform == "win32" or os_name == "posix"`, true, true},
		{`sys_platform == "linux" and platform_machine == "aarch64"`, false, true},
		{`platform_python_implementation != "PyPy"`, true, true},
		{`python_full_version ~= "3.12.0"`, true, true},
		{`extra == "dev"`, false, true},
		{`"linux" in sys_platform`, true, true},
		{`platform_release >= "5"`, false, false}, // packaging raises: not a version
		{`sys.platform == "linux"`, true, true},
	} {
		m, ok := parsePipMarker(tc.marker)
		if !ok {
			t.Errorf("parsePipMarker(%q) failed", tc.marker)
			continue
		}
		holds, know := m.evaluate(env, "")
		if holds != tc.holds || know != tc.know {
			t.Errorf("%q = (%v, %v), want (%v, %v)", tc.marker, holds, know, tc.holds, tc.know)
		}
	}
	if applies, known := pipRequirementApplies(`x ; python_version < "2"`, nil); applies || known {
		t.Error("without an environment a marker is unknown")
	}
	if applies, known := pipRequirementApplies(`x`, nil); !applies || !known {
		t.Error("a requirement without a marker applies")
	}
}

func TestPipWheelCompatible_GEC1021R7(t *testing.T) {
	env := pipMarkerEnv{"python_version": "3.12", "implementation_name": "cpython", "sys_platform": "linux", "platform_machine": "x86_64"}
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"six-1.16.0-py2.py3-none-any.whl", true},
		{"pkg-1.0-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", true},
		{"pkg-1.0-cp311-cp311-manylinux_2_17_x86_64.whl", false},
		{"pkg-1.0-cp38-abi3-manylinux2014_x86_64.whl", true},
		{"pkg-1.0-cp312-cp312-win_amd64.whl", false},
		{"pkg-1.0-cp312-cp312-manylinux2014_aarch64.whl", false},
		{"pkg-1.0-py2-none-any.whl", false},
		{"pkg-1.0-pp310-pypy310_pp73-manylinux2014_x86_64.whl", false},
		{"pkg-1.0-1-py3-none-any.whl", true}, // a build tag
		{"not-a-wheel.tar.gz", false},
	} {
		if got := pipWheelCompatible(tc.file, env); got != tc.want {
			t.Errorf("pipWheelCompatible(%s) = %v, want %v", tc.file, got, tc.want)
		}
	}
	if !pipWheelCompatible("pkg-1.0-cp312-cp312-win_amd64.whl", nil) {
		t.Error("without an environment every wheel counts")
	}
	for _, tc := range [][3]string{{"requests-2.32.3.tar.gz", "requests", "2.32.3"}, {"Foo_Bar-1.0.zip", "foo-bar", "1.0"}, {"foo-bar-1.0rc1.tar.gz", "foo-bar", "1.0rc1"}, {"other-1.0.tar.gz", "foo", ""}} {
		if got, _ := pipSdistVersion(tc[0], tc[1]); got != tc[2] {
			t.Errorf("pipSdistVersion(%s, %s) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

// The index probe reads PEP 691 JSON and PEP 503 HTML pages; its output
// round-trips through parsePipIndexProbe, and a page it cannot read is no
// answer.
func TestPipIndexProbePages_GEC1021R7(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/jsonpkg/":
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = w.Write([]byte(`{"meta":{"api-version":"1.1"},"name":"jsonpkg","files":[` +
				`{"filename":"jsonpkg-1.0.tar.gz","yanked":false},` +
				`{"filename":"jsonpkg-2.0-py3-none-any.whl","yanked":"broken","requires-python":">=3.8"}]}`))
		case "/simple/htmlpkg/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><a href="../../f/htmlpkg-1.0.tar.gz#sha256=ab">htmlpkg-1.0.tar.gz</a>` +
				`<a href="x" data-yanked="" data-requires-python="&gt;=3.9">htmlpkg-2.0-py3-none-any.whl</a></body></html>`))
		case "/simple/badjson/":
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = w.Write([]byte(`{`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	probe := func(page string) ([]pipIndexFile, pipIndexAnswer) {
		var out bytes.Buffer
		runPipIndexProbe(&out, []string{srv.URL + page})
		return parsePipIndexProbe(out.String())
	}
	files, answer := probe("/simple/jsonpkg/")
	if answer != pipIndexListed || len(files) != 2 || files[0] != (pipIndexFile{name: "jsonpkg-1.0.tar.gz"}) ||
		files[1] != (pipIndexFile{name: "jsonpkg-2.0-py3-none-any.whl", yanked: true, requiresPython: ">=3.8"}) {
		t.Fatalf("JSON page: %+v %v", files, answer)
	}
	files, answer = probe("/simple/htmlpkg/")
	if answer != pipIndexListed || len(files) != 2 || files[1] != (pipIndexFile{name: "htmlpkg-2.0-py3-none-any.whl", yanked: true, requiresPython: ">=3.9"}) {
		t.Fatalf("HTML page: %+v %v", files, answer)
	}
	if _, answer := probe("/simple/missing/"); answer != pipIndexNotFound {
		t.Fatalf("missing project: %v, want not found", answer)
	}
	if _, answer := probe("/simple/badjson/"); answer != pipIndexUnknown {
		t.Fatalf("unreadable page: %v, want unknown", answer)
	}
	if _, answer := parsePipIndexProbe("status 200\nfile 0 \"\" x-1.0.tar.gz\n"); answer != pipIndexUnknown {
		t.Fatal("a truncated probe output (no end line) must be no answer")
	}
	if got := redactURL("https://user:tok@pypi.corp.example/simple"); got != "https://pypi.corp.example/simple" {
		t.Fatalf("redactURL = %q", got)
	}
}

func TestPipConfigValues_GEC1021R7(t *testing.T) {
	values := pipConfigValues("; comment\n[global]\nindex_url = https://a.example/simple\nextra-index-url =\n  https://b.example/simple\n  https://c.example/simple\n" +
		"[install]\n--trusted-host: a.example\nno-index = false\n[download]\nindex-url = https://ignored.example/\n")
	if values["index-url"] != "https://a.example/simple" || values["trusted-host"] != "a.example" || values["no-index"] != "false" {
		t.Fatalf("values = %v", values)
	}
	if got := strings.Fields(values["extra-index-url"]); !slices.Equal(got, []string{"https://b.example/simple", "https://c.example/simple"}) {
		t.Fatalf("extra-index-url = %q", values["extra-index-url"])
	}
	if strings.Contains(values["index-url"], "ignored") {
		t.Fatal("the [download] section does not apply to an install")
	}
}

// The go fetch gets go.mod, go.sum and, for go mod tidy, an import-only stub
// of each Go file the go command loads (build constraints, package clause,
// import paths - no code); the modules of local replace directives inside the
// workspace come along, one outside refuses the fetch.
func TestGoFetchSources_GEC1021R7(t *testing.T) {
	stub := string(goImportStub([]byte("//go:build linux && !cgo\n\n// Package x does things.\npackage x\n\nimport (\n\tf \"fmt\"\n\t_ \"embed\"\n\t\"C\"\n)\n\nfunc secret() string { return \"TOKEN\" }\n")))
	for _, want := range []string{"//go:build linux && !cgo\n", "package x\n", "import _ \"fmt\"\n", "import _ \"embed\"\n", "import \"C\"\n"} {
		if !strings.Contains(stub, want) {
			t.Fatalf("stub %q lacks %q", stub, want)
		}
	}
	if strings.Contains(stub, "TOKEN") || strings.Contains(stub, "func") || strings.Contains(stub, "does things") {
		t.Fatalf("stub carries code or comments: %q", stub)
	}
	if bad := string(goImportStub([]byte("package\n"))); !strings.HasSuffix(bad, "package\n") {
		t.Fatalf("a file that does not parse must stay unparseable: %q", bad)
	}

	old := goModParse
	goModParse = func(_, _ string, data []byte) (*goModFile, error) {
		mod := &goModFile{}
		for _, line := range strings.Split(string(data), "\n") {
			if f := strings.Fields(line); len(f) == 4 && f[0] == "replace" && f[2] == "=>" {
				mod.Replace = append(mod.Replace, struct{ Old, New goModVersion }{goModVersion{Path: f[1]}, goModVersion{Path: f[3]}})
			}
		}
		return mod, nil
	}
	t.Cleanup(func() { goModParse = old })
	project := newNeedProject(t,
		"go.mod", "module example.com/x\n\ngo 1.22\n\nreplace example.com/lib => ./lib\n",
		"go.sum", "example.com/y v1.0.0 h1:x=\n",
		"main.go", "package main\n\nimport \"example.com/lib\"\n\nfunc main() { lib.F() }\n",
		"main_test.go", "package main\n\nimport \"testing\"\n",
		"lib/go.mod", "module example.com/lib\n\ngo 1.22\n",
		"lib/lib.go", "package lib\n\nimport \"example.com/z\"\n\nfunc F() {}\n",
		"testdata/skip.go", "package skip\n",
		".hidden/skip.go", "package skip\n",
		"nested/go.mod", "module example.com/nested\n",
		"nested/n.go", "package nested\n",
		"README.md", "secret notes\n")
	if !filepath.IsAbs(fetchToolPath("go", project.Path)) {
		t.Skip("no go command to parse go.mod with")
	}
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var copied []string
	_ = filepath.WalkDir(fetch.stage.dir.path, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(fetch.stage.dir.path, path)
			copied = append(copied, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(copied)
	if want := []string{"go.mod", "go.sum", "lib/go.mod", "lib/lib.go", "main.go", "main_test.go"}; !slices.Equal(copied, want) {
		t.Fatalf("fetch directory holds %v, want %v", copied, want)
	}
	if data, _ := os.ReadFile(fetch.stage.dir.abs("main.go")); strings.Contains(string(data), "lib.F()") {
		t.Fatalf("main.go stub carries code: %q", data)
	}
	if !slices.Equal(fetch.stage.writeBack, []string{"go.mod", "go.sum"}) {
		t.Fatalf("write-back = %v", fetch.stage.writeBack)
	}

	outside := newNeedProject(t, "go.mod", "module example.com/x\n\ngo 1.22\n\nreplace example.com/lib => ../lib\n")
	_, _, _, err = outside.planDependencyStages(ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "download"}}, outside.Path)
	var refusal *fetchRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.reason, "outside the project") {
		t.Fatalf("err = %v, want the replacement outside the project refused", err)
	}
}

// The cargo fetch gets every Cargo.toml and Cargo.lock and an empty stub at
// each target path cargo needs - never a source file's contents, never .cargo.
func TestCargoFetchSourcesStubs_GEC1021R7(t *testing.T) {
	project := newNeedProject(t,
		"Cargo.toml", "[workspace]\nmembers = [\"crates/*\"]\n",
		"Cargo.lock", "version = 3\n",
		"crates/a/Cargo.toml", "[package]\nname = \"a\"\nversion = \"0.1.0\"\nbuild = \"gen.rs\"\n\n[[bin]]\nname = \"tool\"\npath = \"tools/tool.rs\"\n\n[lib]\npath = \"src/a.rs\"\n",
		"crates/a/gen.rs", "fn main() { secret() }\n",
		"crates/a/tools/tool.rs", "fn main() {}\n",
		"crates/a/src/a.rs", "pub fn secret() {}\n",
		"crates/a/src/bin/extra.rs", "fn main() {}\n",
		"crates/a/src/bin/multi/main.rs", "fn main() {}\n",
		"crates/b/Cargo.toml", "[package]\nname = \"b\"\nversion = \"0.1.0\"\n",
		"crates/b/src/main.rs", "fn main() {}\n",
		"crates/b/src/other.rs", "mod x;\n",
		".cargo/config.toml", "[alias]\nfetch = \"build\"\n",
		"target/debug/Cargo.toml", "[package]\nname = \"stale\"\n")
	fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"build"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if build == nil || !slices.Contains(build.Args, "--offline") {
		t.Fatalf("build = %+v, want the offline build", build)
	}
	var copied []string
	_ = filepath.WalkDir(fetch.stage.dir.path, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(fetch.stage.dir.path, path)
			copied = append(copied, filepath.ToSlash(rel))
			if !strings.HasSuffix(rel, "Cargo.toml") && !strings.HasSuffix(rel, "Cargo.lock") {
				if info, _ := d.Info(); info.Size() != 0 {
					t.Errorf("%s is not an empty stub", rel)
				}
			}
		}
		return nil
	})
	slices.Sort(copied)
	want := []string{"Cargo.lock", "Cargo.toml", "crates/a/Cargo.toml", "crates/a/gen.rs", "crates/a/src/a.rs", "crates/a/src/bin/extra.rs", "crates/a/src/bin/multi/main.rs", "crates/a/tools/tool.rs", "crates/b/Cargo.toml", "crates/b/src/main.rs"}
	if !slices.Equal(copied, want) {
		t.Fatalf("fetch directory holds %v, want %v", copied, want)
	}
}

// node_modules the fetch produced replaces the workspace's through its
// os.Root handle: links are recreated as links, never followed, and a
// workspace node_modules a loopback step replaced with a symlink (into .git)
// is replaced, not written through.
func TestPlaceFetchTree_GEC1021R7(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, ".git", "hooks", "keep"), "")
	if err := os.Symlink(".git/hooks", filepath.Join(ws, "node_modules")); err != nil {
		t.Fatal(err)
	}
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
	writeFile(t, dir.abs("node_modules/left-pad/package.json"), `{"name":"left-pad"}`)
	writeExecScript(t, dir.abs("node_modules/left-pad/bin/cli.js"), "exit 0\n")
	if err := os.MkdirAll(dir.abs("node_modules/.bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../left-pad/bin/cli.js", dir.abs("node_modules/.bin/left-pad")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", dir.abs("node_modules/left-pad/evil")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := placeFetchTree(dir, root, "node_modules"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(ws, "node_modules")); err != nil || !info.IsDir() {
		t.Fatalf("node_modules is %v (%v), want a real directory", info, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(ws, ".git", "hooks")); len(entries) != 1 {
		t.Fatalf("wrote through the planted node_modules symlink into .git/hooks: %v", entries)
	}
	if link, err := os.Readlink(filepath.Join(ws, "node_modules", ".bin", "left-pad")); err != nil || link != "../left-pad/bin/cli.js" {
		t.Fatalf(".bin link = %q (%v)", link, err)
	}
	if link, _ := os.Readlink(filepath.Join(ws, "node_modules", "left-pad", "evil")); link != "/etc/passwd" {
		t.Fatalf("a link must be recreated as it was, never followed: %q", link)
	}
	if info, err := os.Stat(filepath.Join(ws, "node_modules", "left-pad", "bin", "cli.js")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the executable bit was lost: %v %v", info, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(ws, ".makewand-node_modules-*")); len(matches) != 0 {
		t.Fatalf("staging left behind: %v", matches)
	}
}

// A file the fetch produced is put into the workspace only as a regular file:
// a symlink at its name is refused, never followed out of the workspace.
func TestWriteBackRefusesSymlink_GEC1021R7(t *testing.T) {
	ws := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	writeFile(t, victim, "original")
	if err := os.Symlink(victim, filepath.Join(ws, "go.sum")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeBackWorkspaceFile(root, "go.sum", []byte("new")); err == nil {
		t.Fatal("writeBackWorkspaceFile replaced a symlink; want a refusal")
	}
	if data, _ := os.ReadFile(victim); string(data) != "original" {
		t.Fatalf("victim overwritten: %q", data)
	}
	if err := writeBackWorkspaceFile(root, "Cargo.lock", []byte("lock")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(ws, "Cargo.lock")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("new Cargo.lock: %v %v, want 0644", info, err)
	}
}

// Every loopback step reads the shared dependency caches read-only - never
// bound writable - with CARGO_HOME pointed at the cargo cache and the module
// cache as the first local GOPROXY entry.
func TestLoopbackStepsReadFetchCachesReadOnly_GEC1021R7(t *testing.T) {
	fakeWorkingBwrap(t)
	stubGoEnv(t, hostGoEnv{GOVERSION: "go1.27.1", GOPROXY: "https://proxy.golang.org,direct"}, nil)
	project := newNeedProject(t, "go.mod", "module example.com/x\n\ngo 1.22\n")
	area, err := openDepFetchArea(project.Path)
	if err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(area.cacheDir(fetchCacheGo), "cache", "download")
	area.close()
	if err := os.MkdirAll(download, 0o700); err != nil {
		t.Fatal(err)
	}
	verifyGetenv = func(key string) string {
		if key == "PATH" {
			if goBin, err := exec.LookPath("go"); err == nil {
				return filepath.Dir(goBin) + ":/usr/bin:/bin"
			}
			return "/usr/bin:/bin"
		}
		return ""
	}
	env := verifyExecEnvironment{mode: verifyExecIsolated, bwrapPath: "/usr/bin/bwrap"}
	_, args, err := project.wrapSandboxStep(context.Background(), env, project.Path, ExecPlan{Kind: "tests", Command: "go", Args: []string{"test", "./..."}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--ro-bind "+area.cache+" "+area.cache) {
		t.Fatalf("the shared caches are not bound read-only: %q", joined)
	}
	if strings.Contains(joined, "--bind "+area.cache+" ") {
		t.Fatalf("a loopback step binds the shared caches writable: %q", joined)
	}
	if !strings.Contains(joined, "--setenv CARGO_HOME "+filepath.Join(area.cache, fetchCacheCargo)) {
		t.Fatalf("CARGO_HOME is not the shared cargo cache: %q", joined)
	}
	if lookPathIn("go", verifyGetenv("PATH")) != "" && !strings.Contains(joined, "--setenv GOPROXY file://"+download+",") {
		t.Fatalf("GOPROXY does not start with the shared module cache: %q", joined)
	}
}

// npm's networked install is `npm ci` of the inspected lockfile with the
// plan's own options that choose what to install, never one that writes the
// lockfile.
func TestNpmInstallArgs_GEC1021R7(t *testing.T) {
	if got := npmInstallArgs([]string{"install", "--ignore-scripts", "--no-package-lock", "--omit=dev", "--no-save"}, true); !slices.Equal(got, []string{"ci", "--ignore-scripts", "--no-audit", "--no-fund", "--omit=dev"}) {
		t.Fatalf("npmInstallArgs = %q", got)
	}
	if got := npmInstallArgs([]string{"install"}, false); got[0] != "install" {
		t.Fatalf("without a manifest npm keeps its own subcommand: %q", got)
	}
}

// Live, with the real pip, bubblewrap, slirp4netns and PyPI: a wheel is
// downloaded in the fetch directory and installed offline from it.
func TestPipFetchThenOfflineInstallLive_GEC1021R7(t *testing.T) {
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
	writeProjectFiles(t, project.Path, "requirements.txt", "six==1.16.0\n")
	// --target keeps the install in the workspace whatever kind of Python the
	// host pip belongs to (a virtualenv refuses --user).
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--target", "pylib", "-r", "requirements.txt"}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if _, err := project.ReadFile(filepath.Join("pylib", "six.py")); err != nil {
		t.Fatalf("six was not installed offline from the wheelhouse: %v\n%s", err, result.Stdout)
	}
}

// Live, with the real npm (12 or later), bubblewrap, slirp4netns and the npm
// registry: the inspected lockfile is installed in the fetch directory and
// the host puts node_modules and the lockfile into the workspace.
func TestNpmFetchPlacesNodeModulesLive_GEC1021R7(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("no npm on PATH")
	}
	if npmMajorVersion(npm) < npmNetworkedLockMajor {
		t.Skipf("%s is not npm %d or later", npm, npmNetworkedLockMajor)
	}
	project := sandboxDepsProject(t)
	requireUserModeNetwork(t)
	unsetProxyEnv(t)
	t.Setenv("PATH", filepath.Dir(npm)+":/usr/bin:/bin")
	liveSandbox(t)
	if !hostReaches("https://registry.npmjs.org/left-pad") {
		t.Skip("the npm registry is not reachable from this host")
	}
	writeProjectFiles(t, project.Path, "package.json", `{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`)
	plan := ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}, env: []string{"npm_config_update_notifier=false"}}
	result, err := project.RunVerificationPlan(context.Background(), plan)
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if _, err := project.ReadFile(filepath.Join("node_modules", "left-pad", "package.json")); err != nil {
		t.Fatalf("node_modules/left-pad was not placed in the project: %v", err)
	}
	if lock, err := project.ReadFile("package-lock.json"); err != nil || !strings.Contains(lock, "left-pad-1.3.0.tgz") {
		t.Fatalf("package-lock.json = %q (%v), want the resolved lockfile", lock, err)
	}
}

// hostReaches reports whether the host gets an answer from url directly.
func hostReaches(url string) bool {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Head(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// Live, with the real cargo, bubblewrap, slirp4netns and crates.io: cargo
// fetch runs on the copied manifests, the host puts the Cargo.lock it wrote
// into the workspace, and the offline build reads the crates from the shared
// cargo cache read-only.
func TestCargoFetchThenOfflineBuildLive_GEC1021R7(t *testing.T) {
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
	if !hostReaches("https://index.crates.io/config.json") {
		t.Skip("crates.io is not reachable from this host")
	}
	writeProjectFiles(t, project.Path,
		"Cargo.toml", "[package]\nname = \"x\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nitoa = \"1\"\n",
		"src/main.rs", "fn main() { println!(\"{}\", itoa::Buffer::new().format(7)); }\n")
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Detector: "Cargo.toml", Command: "cargo", Args: []string{"build"}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if lock, err := project.ReadFile("Cargo.lock"); err != nil || !strings.Contains(lock, "name = \"itoa\"") {
		t.Fatalf("Cargo.lock = %q (%v), want the lockfile the fetch wrote", lock, err)
	}
}

// Live, with the real go: go mod tidy runs on the copied go.mod and the
// import-only stubs and keeps exactly the requirements the packages import -
// here of local replacement modules, so nothing is downloaded - and the host
// puts the tidied go.mod into the workspace.
func TestGoTidyThroughImportStubsLive_GEC1021R7(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	requireUserModeNetwork(t)
	disableGoTelemetryAt(t, os.Getenv("HOME"))
	t.Setenv("PATH", filepath.Dir(goBin)+":/usr/bin:/bin")
	t.Setenv("GOMODCACHE", filepath.Join(t.TempDir(), "modcache"))
	t.Setenv("GOPATH", filepath.Join(t.TempDir(), "gopath"))
	for _, key := range []string{"GOPROXY", "GOSUMDB", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GOFLAGS", "GOENV"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	unsetProxyEnv(t)
	liveSandbox(t)
	project, err := NewProject("go-tidy-live", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFiles(t, project.Path,
		"go.mod", "module example.com/app\n\ngo 1.22\n\nrequire (\n\texample.com/used v0.0.0\n\texample.com/unused v0.0.0\n)\n\nreplace example.com/used => ./used\n\nreplace example.com/unused => ./unused\n",
		"main.go", "package main\n\nimport \"example.com/used\"\n\nfunc main() { used.F() }\n",
		"used/go.mod", "module example.com/used\n\ngo 1.22\n",
		"used/used.go", "package used\n\nfunc F() {}\n",
		"unused/go.mod", "module example.com/unused\n\ngo 1.22\n",
		"unused/unused.go", "package unused\n")
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Detector: "go.mod", Command: "go", Args: []string{"mod", "tidy"}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	gomod, err := project.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gomod, "example.com/used v0.0.0") || strings.Contains(gomod, "example.com/unused v0.0.0") {
		t.Fatalf("tidied go.mod = %q, want the imported requirement kept and the unused one dropped", gomod)
	}
	if data, _ := os.ReadFile(filepath.Join(project.Path, "main.go")); !bytes.Contains(data, []byte("used.F()")) {
		t.Fatal("the workspace's own source changed")
	}
}

// The networked go fetch sees only module files and import stubs, so it runs
// only `go mod download` and `go mod tidy`; another go dependency step is
// refused with the network rather than failing in the fetch directory.
func TestGoFetchOnlyModSubcommands_GEC1021R7(t *testing.T) {
	old := userNetLookPath
	userNetLookPath = func(string) (string, error) { return "/usr/bin/slirp4netns", nil }
	t.Cleanup(func() { userNetLookPath = old })
	project := newNeedProject(t, "go.mod", "module example.com/x\n\ngo 1.22\n")
	for _, args := range [][]string{{"mod", "tidy"}, {"mod", "download", "-x"}} {
		if reason := project.networkFetchUnavailable(ExecPlan{Kind: "deps", Command: "go", Args: args}); reason != "" {
			t.Errorf("go %v refused: %s", args, reason)
		}
	}
	for _, args := range [][]string{{"get", "example.com/y@v1.0.0"}, {"mod", "vendor"}, {"build", "./..."}, nil} {
		if reason := project.networkFetchUnavailable(ExecPlan{Kind: "deps", Command: "go", Args: args}); !strings.Contains(reason, "go mod download") {
			t.Errorf("go %v: refusal %q, want one naming the go subcommands a fetch runs", args, reason)
		}
	}
}
