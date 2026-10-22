//go:build linux

package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/trustedbin"
)

// GEC-1021-1: the dependency step is split so only downloading uses the
// network. A networked fetch runs no project code (pip downloads wheels only,
// npm --ignore-scripts, cargo fetch, go mod tidy/download) and is built without
// the host module caches or workspace secrets; the build/install that may run
// project code (pip install -e ., cargo build) runs loopback-only from what was
// fetched. eng-1021-4: a sandboxed go step leaves no telemetry files.

// fetchWheelhouse returns the wheelhouse a pip download plan writes to (its
// --dest) and checks it sits in the fetch's own directory outside the
// workspace, never in it or in the persistent sandbox HOME (GEC-1021-1 round
// 7: a networked fetch sees no part of the workspace).
func fetchWheelhouse(t *testing.T, projectDir string, fetch *ExecPlan) string {
	t.Helper()
	wh := ""
	for i, a := range fetch.Args {
		if a == "--dest" && i+1 < len(fetch.Args) {
			wh = fetch.Args[i+1]
		}
	}
	if wh == "" {
		t.Fatalf("pip fetch has no --dest: %v", fetch.Args)
	}
	if pathWithin(projectDir, wh) {
		t.Fatalf("wheelhouse %q is inside the workspace %q, want the fetch directory", wh, projectDir)
	}
	if fetch.stage == nil || wh != fetch.stage.dir.abs("wheelhouse") {
		t.Fatalf("wheelhouse %q is not in the fetch's own directory", wh)
	}
	return wh
}

// Each ecosystem's fetch/build command lines are constructed correctly, and the
// step that may run project code is the loopback build, never the networked
// fetch.
func TestDependencyStagesSplit_GEC1021(t *testing.T) {
	t.Run("npm ensures --ignore-scripts and needs no build", func(t *testing.T) {
		project := newNeedProject(t, "package.json", `{"name":"x"}`)
		for _, args := range [][]string{{"install"}, {"install", "--ignore-scripts", "--no-package-lock"}} {
			fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "npm", Args: args}, project.Path)
			if err != nil {
				t.Fatal(err)
			}
			cleanup()
			if build != nil {
				t.Fatalf("npm build stage = %+v, want none (lifecycle scripts never run)", build)
			}
			if fetch == nil || !slices.Contains(fetch.Args, "--ignore-scripts") {
				t.Fatalf("npm fetch = %+v, want --ignore-scripts so no project code runs", fetch)
			}
		}
	})

	t.Run("go mod tidy is a fetch, no build", func(t *testing.T) {
		project := newNeedProject(t, "go.mod", "module x\n\ngo 1.22\n")
		fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if build != nil || fetch == nil || !slices.Equal(fetch.Args, []string{"mod", "tidy"}) {
			t.Fatalf("go stages fetch=%+v build=%+v, want a lone `go mod tidy` fetch", fetch, build)
		}
	})

	t.Run("cargo build fetches then builds offline", func(t *testing.T) {
		project := newNeedProject(t, "Cargo.toml", "[package]\nname=\"x\"\n")
		for _, tc := range []struct {
			in        []string
			wantFetch []string
			wantBuild []string
		}{
			{[]string{"build"}, []string{"fetch"}, []string{"build", "--offline"}},
			{[]string{"build", "--locked"}, []string{"fetch", "--locked"}, []string{"build", "--locked", "--offline"}},
		} {
			fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: tc.in}, project.Path)
			if err != nil {
				t.Fatal(err)
			}
			cleanup()
			if fetch == nil || fetch.Command != "cargo" || !slices.Equal(fetch.Args, tc.wantFetch) {
				t.Fatalf("cargo %v fetch = %+v, want %v", tc.in, fetch, tc.wantFetch)
			}
			if build == nil || !slices.Equal(build.Args, tc.wantBuild) {
				t.Fatalf("cargo %v build = %+v, want %v (offline)", tc.in, build, tc.wantBuild)
			}
		}
		// A bare `cargo fetch` (the prefetch step) is the whole fetch.
		fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"fetch"}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if build != nil || fetch == nil || !slices.Equal(fetch.Args, []string{"fetch"}) {
			t.Fatalf("cargo fetch stages fetch=%+v build=%+v, want a lone fetch", fetch, build)
		}
	})

	t.Run("pip -r downloads wheels then installs offline", func(t *testing.T) {
		project := newNeedProject(t, "requirements.txt", "six==1.16.0\n")
		fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		wh := fetchWheelhouse(t, project.Path, fetch)
		// The requirement file itself is never handed to the networked download
		// (pip would build any local/sdist target it lists); its named index
		// requirements are passed instead.
		wantFetch := []string{"download", "--only-binary=:all:", "--no-input", "--dest", wh, "six==1.16.0"}
		if fetch == nil || !slices.Equal(fetch.Args, wantFetch) {
			t.Fatalf("pip -r fetch = %+v, want %v (named requirements, not the file)", fetch, wantFetch)
		}
		wantBuild := []string{"install", "--no-index", "--find-links", wh, "--user", "-r", "requirements.txt"}
		if build == nil || !slices.Equal(build.Args, wantBuild) {
			t.Fatalf("pip -r build = %+v, want %v (offline, from the wheelhouse)", build, wantBuild)
		}
	})

	t.Run("pip -e . downloads build requires and static deps", func(t *testing.T) {
		project := newNeedProject(t, "pyproject.toml",
			"[build-system]\nrequires = [\"hatchling>=1\"]\nbuild-backend = \"hatchling.build\"\n\n[project]\nname = \"x\"\nversion = \"0\"\ndependencies = [\"requests>=2\"]\n")
		fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-e", ".", "--use-pep517"}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		wh := fetchWheelhouse(t, project.Path, fetch)
		wantFetch := []string{"download", "--only-binary=:all:", "--no-input", "--dest", wh, "hatchling>=1", "requests>=2"}
		if fetch == nil || !slices.Equal(fetch.Args, wantFetch) {
			t.Fatalf("pip -e . fetch = %+v, want %v (build requires + static deps, wheels only)", fetch, wantFetch)
		}
		wantBuild := []string{"install", "--no-index", "--find-links", wh, "--user", "-e", ".", "--use-pep517"}
		if build == nil || !slices.Equal(build.Args, wantBuild) {
			t.Fatalf("pip -e . build = %+v, want %v (offline editable build)", build, wantBuild)
		}
		// The wheelhouse exists while the stages run and is removed by cleanup.
		if _, err := os.Stat(wh); err != nil {
			t.Fatalf("wheelhouse not created: %v", err)
		}
		cleanup()
		if _, err := os.Stat(wh); !os.IsNotExist(err) {
			t.Fatalf("wheelhouse not cleaned up: %v", err)
		}
	})

	t.Run("pyproject without build-system falls back to setuptools+wheel", func(t *testing.T) {
		project := newNeedProject(t, "pyproject.toml", "[project]\nname = \"x\"\nversion = \"0\"\n")
		fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-e", "."}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if fetch == nil || !slices.Contains(fetch.Args, "setuptools>=40.8.0") || !slices.Contains(fetch.Args, "wheel") {
			t.Fatalf("pip -e . fetch = %+v, want the PEP 518 default build requires", fetch)
		}
	})
}

// A buildable target in a requirement/constraint file, a build-system require
// or a direct reference must never reach the networked `pip download`: pip
// builds such a target to read its metadata even under --only-binary=:all:,
// running project code while the network is available (GEC-1021-1). The raw
// requirement/constraint file is never handed to the fetch, and the loopback
// build keeps the original command line so the local project still installs.
func TestNetworkedFetchExcludesBuildableTargets_GEC1021(t *testing.T) {
	// The fetch must contain none of these: raw requirement/constraint files,
	// local paths, archives, URLs, VCS or `name @ url` references.
	forbidden := func(t *testing.T, fetch *ExecPlan) {
		t.Helper()
		for _, arg := range fetch.Args {
			switch {
			case arg == "-r" || arg == "-c" || arg == "--requirement" || arg == "--constraint",
				strings.HasSuffix(arg, ".txt"),
				arg == "." || arg == "-e" || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../"),
				strings.Contains(arg, "://"), strings.Contains(arg, "@"),
				strings.HasSuffix(arg, ".tar.gz") || strings.HasSuffix(arg, ".zip"):
				t.Fatalf("networked fetch args must not include a buildable target or a requirement file, got %q in %v", arg, fetch.Args)
			}
		}
	}

	t.Run("requirements.txt mixing named requirements and buildable targets", func(t *testing.T) {
		reqs := strings.Join([]string{
			"flask==2.0",                         // named: fetched
			".",                                  // local project: built loopback
			"-e ./pkg",                           // local editable
			"https://example.invalid/pkg.tar.gz", // URL
			"./local.tar.gz",                     // local archive
			"evilpkg @ file:///tmp/evil",         // direct reference
		}, "\n")
		project := newNeedProject(t, "requirements.txt", reqs+"\n")
		fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if fetch == nil || !slices.Contains(fetch.Args, "flask==2.0") {
			t.Fatalf("fetch = %+v, want the named requirement flask==2.0", fetch)
		}
		forbidden(t, fetch)
		// The offline build still receives the requirement file, so the local
		// project is installed loopback-only from it.
		if build == nil || !slices.Contains(build.Args, "-r") || !slices.Contains(build.Args, "requirements.txt") || !slices.Contains(build.Args, "--no-index") {
			t.Fatalf("build = %+v, want the offline install to keep -r requirements.txt", build)
		}
	})

	t.Run("constraint file is not handed to the fetch", func(t *testing.T) {
		project := newNeedProject(t, "requirements.txt", "flask==2.0\n", "constraints.txt", ".\n")
		fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-r", "requirements.txt", "-c", "constraints.txt"}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if fetch == nil {
			t.Fatal("fetch = nil, want a download for flask==2.0")
		}
		forbidden(t, fetch)
		if !slices.Contains(build.Args, "-c") || !slices.Contains(build.Args, "constraints.txt") {
			t.Fatalf("build = %+v, want the offline install to keep -c constraints.txt", build)
		}
	})

	t.Run("a malicious build-system require is not fetched", func(t *testing.T) {
		project := newNeedProject(t, "pyproject.toml",
			"[build-system]\nrequires = [\"evil @ file:///tmp/evil\", \"hatchling>=1\"]\nbuild-backend = \"hatchling.build\"\n")
		fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-e", "."}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if fetch == nil || !slices.Contains(fetch.Args, "hatchling>=1") {
			t.Fatalf("fetch = %+v, want the named build requirement hatchling>=1", fetch)
		}
		forbidden(t, fetch)
	})

	t.Run("a local-only install runs no networked step", func(t *testing.T) {
		// requirements.txt = `.` and a pyproject whose in-tree backend needs no
		// index build requirement: nothing resolves from an index, so the fetch
		// is skipped entirely and the backend can never run with the network.
		project := newNeedProject(t, "requirements.txt", ".\n", "pyproject.toml",
			"[build-system]\nrequires = []\nbackend-path = [\"_b\"]\nbuild-backend = \"b\"\n")
		fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if fetch != nil {
			t.Fatalf("fetch = %+v, want no networked step when nothing resolves from an index", fetch)
		}
		if build == nil || !slices.Contains(build.Args, "--no-index") {
			t.Fatalf("build = %+v, want an offline install", build)
		}
	})
}

// A networked fetch is built without the host Go module cache: it downloads
// into the sandbox's own cache instead of reading the host's (which can hold
// private modules). A loopback step keeps the host cache (no network to
// exfiltrate it).
func TestNetworkedFetchLayoutOmitsHostModuleCache_GEC1021(t *testing.T) {
	home, pathEnv := fakeToolchainHome(t)
	download := filepath.Join(home, "gopath", "pkg", "mod", "cache", "download")
	stubGoEnv(t, hostGoEnv{
		GOROOT:     filepath.Join(home, "sdk", "go1.27"),
		GOMODCACHE: filepath.Join(home, "gopath", "pkg", "mod"),
		GOVERSION:  "go1.27.1",
		GOPROXY:    "https://proxy.golang.org,direct",
	}, nil)
	workspace := filepath.Join(home, "dev", "proj")

	loopback := buildSandboxHomeLayout(home, workspace, pathEnv, func(string) string { return "" }, false)
	if !slices.Contains(roBinds(loopback.beforeWorkspace), download) {
		t.Fatalf("loopback layout must bind the host module cache: %v", loopback.beforeWorkspace)
	}
	if proxy := envValue(loopback, "GOPROXY"); !strings.Contains(proxy, "file://") {
		t.Fatalf("loopback GOPROXY = %q, want the host cache as a local proxy", proxy)
	}

	fetch := buildSandboxHomeLayout(home, workspace, pathEnv, func(string) string { return "" }, true)
	if slices.Contains(roBinds(fetch.beforeWorkspace), download) {
		t.Fatalf("networked fetch must not bind the host module cache: %v", fetch.beforeWorkspace)
	}
	if proxy := envValue(fetch, "GOPROXY"); strings.Contains(proxy, "file://") {
		t.Fatalf("networked fetch GOPROXY = %q, want upstream only (no host cache)", proxy)
	} else if proxy != "https://proxy.golang.org,direct" {
		t.Fatalf("networked fetch GOPROXY = %q, want the host's upstream proxies", proxy)
	}
}

// The classifier: pip reporting no installable distribution (a source-only
// dependency the wheel-only fetch cannot get) is an environment limitation, not
// a candidate failure, and is not mistaken for a registry outage.
func TestUnfetchableDependencyClassification_GEC1021(t *testing.T) {
	project := newNeedProject(t, "requirements.txt", "six\n")
	plan := ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-r", "requirements.txt"}}

	sdistOnly := &ExecResult{ExitCode: 1, Stderr: "ERROR: Could not find a version that satisfies the requirement examplepkg (from versions: 1.0.0, 2.0.0)\nERROR: No matching distribution found for examplepkg"}
	if msg := project.unfetchableDependency(plan, sdistOnly); msg == "" || !strings.Contains(msg, "examplepkg") || !strings.Contains(msg, "wheel") {
		t.Fatalf("unfetchableDependency = %q, want a clear message naming examplepkg", msg)
	}
	if project.dependencyOutageSuspected(plan, sdistOnly) {
		t.Fatal("a reachable index with no wheel must not be mistaken for a registry outage")
	}

	// A real outage prints a connection failure too: that stays an outage for
	// RunVerificationPlan's connectivity check to confirm, not unfetchable.
	outage := &ExecResult{ExitCode: 1, Stderr: pipIndexUnreachable}
	if !project.dependencyOutageSuspected(plan, outage) {
		t.Fatalf("an index connection failure should be suspected as an outage: %q", pipIndexUnreachable)
	}

	// A non-pip step never reports unfetchable (only pip fetches wheels).
	if msg := project.unfetchableDependency(ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}}, sdistOnly); msg != "" {
		t.Fatalf("go step unfetchableDependency = %q, want none", msg)
	}
}

// Live: in the real sandbox, the dependency build/install runs loopback-only
// while the fetch gets outbound network. A fake pip (outside the workspace)
// reports, per subcommand, whether it saw a default route (outbound network):
// the download does, the install does not. Each run fails at the step it
// observes, so that step's output is the result.
func TestDependencyBuildStepRunsWithoutNetwork_GEC1021(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	requireUserModeNetwork(t)

	project, err := NewProject("fetch-only", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := outsideTools(t)
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	if err := project.WriteFiles([]ExtractedFile{{Path: "requirements.txt", Content: "six==1.16.0\n"}}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"download", "install"} {
		pip := `net=no
/bin/grep -q '	00000000	' /proc/net/route 2>/dev/null && net=yes
[ "$1" = ` + step + ` ] && { echo "$1 net=$net"; exit 1; }
exit 0
`
		writeExecScript(t, filepath.Join(tools, "pip"), pip)
		result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}})
		if err != nil || result == nil {
			t.Fatalf("RunVerificationPlan (%s): %+v, %v", step, result, err)
		}
		want := map[string]string{"download": "download net=yes", "install": "install net=no"}[step]
		if got := strings.TrimSpace(result.Stdout); got != want {
			t.Fatalf("%s step: %q, want %q", step, got, want)
		}
	}
}

// Live: a sandboxed go step leaves no telemetry counter or upload files, so
// nothing can be written under the sandbox HOME after the step returns and race
// t.TempDir cleanup (eng-1021-4). Only the mode file (off) remains.
func TestSandboxedGoStepLeavesNoTelemetry_eng1021_4(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	goSandboxForTest(t)
	project := goForgeProject(t)
	if _, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "tests", Command: "go", Args: []string{"test", "-json", "-count=1", "./..."}}); err != nil {
		t.Fatalf("RunVerificationPlan: %v", err)
	}
	telemetry := filepath.Join(project.Path, filepath.FromSlash(sandboxHomeRelPath), ".config", "go", "telemetry")
	entries, err := os.ReadDir(telemetry)
	if err != nil {
		t.Fatalf("read sandbox telemetry dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "mode" {
			t.Fatalf("sandbox telemetry left %q behind; want telemetry off (only the mode file)", entry.Name())
		}
	}
	mode, err := os.ReadFile(filepath.Join(telemetry, "mode"))
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(mode)), "off") {
		t.Fatalf("telemetry mode = %q (err %v), want off", mode, err)
	}
}

// requireUserModeNetwork skips when the user-mode network helper is not
// available, since the networked-fetch distinction needs it.
func requireUserModeNetwork(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "1G")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	if _, err := trustedbin.LookPath(userNetHelperName); err != nil {
		t.Skipf("slirp4netns unavailable: %v", err)
	}
}
