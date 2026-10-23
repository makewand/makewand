package engine

import (
	"context"
	"errors"
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// fetchStage is the manifest-only sandbox of the networked stage of a split
// dependency plan (see sandbox_fetchdir.go).
type fetchStage struct {
	area *depFetchArea
	dir  *fetchDir
	// writeBack lists the files the fetch produces that belong in the
	// workspace (go.mod and go.sum after go mod tidy, Cargo.lock), put there
	// by the host once the fetch succeeded.
	writeBack []string
	// pip holds the package sources a pip fetch used, for classifying a
	// requirement pip found no distribution for.
	pip pipFetchSources
}

// fetchRefusal is a dependency plan makewand will not run with the network:
// an environment limitation of fetch-only networking, never a candidate
// failure.
type fetchRefusal struct{ reason string }

func (r *fetchRefusal) Error() string { return r.reason }

// refuseFetch reports a refused networked fetch: a result carrying the reason
// and an error wrapping ErrDependencyNetworkUnavailable.
func refuseFetch(result *ExecResult, network sandboxNetwork, reason string) (*ExecResult, sandboxNetwork, error) {
	if result == nil {
		result = &ExecResult{ExitCode: 1}
	}
	result.Note = reason
	return result, network, fmt.Errorf("%w (%s)", ErrDependencyNetworkUnavailable, reason)
}

// execLoopbackStep runs one verification command in the workspace sandbox,
// loopback-only. offline disables npm's registry retries for a dependency
// install that has no outbound network, so a needed download fails fast.
func (p *Project) execLoopbackStep(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string, offline bool) (*ExecResult, error) {
	wrappedCmd, wrappedArgs, err := p.wrapSandboxStep(ctx, env, projectDir, plan, readOnly)
	if err != nil {
		return nil, err
	}
	if offline {
		wrappedArgs = insertBeforeCommand(wrappedArgs, len(plan.Args), offlineDependencySettings()...)
	}
	return p.execSandboxed(ctx, wrappedCmd, wrappedArgs, nil)
}

// wrapSandboxStep builds the bwrap argv of a loopback step and applies the
// per-run settings (acceptance isolation, the shared dependency caches, the
// plan environment) and the go toolchain precondition.
func (p *Project) wrapSandboxStep(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string) (string, []string, error) {
	cacheBinds, cacheEnv := loopbackFetchCache(projectDir)
	readOnly = append(append(slices.Clone(readOnly), plan.binds...), cacheBinds...)
	wrappedCmd, wrappedArgs, err := wrapVerificationCommand(env.bwrapPath, projectDir, plan.Command, plan.Args, verificationPlanAllowsNetwork(plan), readOnly)
	if err != nil {
		return "", nil, fmt.Errorf("verification sandbox: %w", err)
	}
	if acceptancePolicy(ctx) != nil {
		// Local diagnostics must not introduce undeclared cache files into the
		// execution tree later sealed by independent acceptance.
		settings := []string{"--setenv", "PYTHONDONTWRITEBYTECODE", "1", "--setenv", "PYTHONPYCACHEPREFIX", "/tmp/makewand-bytecode", "--setenv", "PYTEST_ADDOPTS", "-p no:cacheprovider", "--setenv", "CARGO_TARGET_DIR", filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath), "cargo-target")}
		wrappedArgs = insertBeforeCommand(wrappedArgs, len(plan.Args), settings...)
	}
	// The plan's own settings come last, so they win.
	for _, setting := range append(cacheEnv, plan.env...) {
		if key, value, ok := strings.Cut(setting, "="); ok {
			wrappedArgs = insertBeforeCommand(wrappedArgs, len(plan.Args), "--setenv", key, value)
		}
	}
	if plan.Command == "go" {
		// The sandbox pins GOTOOLCHAIN=local; report an unsatisfiable go.mod
		// requirement as an environment problem instead of letting it surface
		// as a failed (candidate-attributed) test run.
		if err := checkSandboxGoToolchain(projectDir, verifyHomeLayout(projectDir, sandboxPathEnv(), false).goVersion); err != nil {
			return "", nil, err
		}
	}
	return wrappedCmd, wrappedArgs, nil
}

// loopbackFetchCache returns what a loopback step gets of the shared
// dependency caches the networked fetches fill: a read-only bind, CARGO_HOME
// pointed at the cargo cache, and the go module cache as the first, local
// entry of GOPROXY (the go command extracts from it into the sandbox HOME's
// own module cache, verified against go.sum). Without a usable fetch area the
// step keeps the sandbox HOME's caches alone.
func loopbackFetchCache(projectDir string) (readOnly, env []string) {
	area, err := openDepFetchArea(projectDir)
	if err != nil {
		return nil, nil
	}
	defer area.close()
	env = []string{"CARGO_HOME=" + area.cacheDir(fetchCacheCargo)}
	download := filepath.Join(area.cacheDir(fetchCacheGo), "cache", "download")
	if info, err := os.Stat(download); err == nil && info.IsDir() {
		if goproxy := layoutEnvValue(verifyHomeLayout(projectDir, sandboxPathEnv(), false), "GOPROXY"); goproxy != "" {
			env = append(env, "GOPROXY=file://"+filepath.ToSlash(download)+","+goproxy)
		}
	}
	return []string{area.cache}, env
}

// layoutEnvValue returns the value the layout environment sets for key.
func layoutEnvValue(layout sandboxHomeLayout, key string) string {
	for i := 0; i+1 < len(layout.env); i += 2 {
		if layout.env[i] == key {
			return layout.env[i+1]
		}
	}
	return ""
}

// execDependencyPlan runs a dependency plan as the owner's 2026-10-21 decision
// requires: only downloading uses the network, and no project-controlled code
// runs while a step has it. With the user-mode network helper, the networked
// fetch runs in a manifest-only fetch directory that sees no part of the
// workspace (sandbox_fetchdir.go); a loopback-only build then installs from
// what was fetched, running any project code (setup.py, build.rs) with no
// network. Without the helper nothing can download, so the plan runs as a
// single loopback-only step, as it always did. The failed step's result and
// the fetch's network are returned so RunVerificationPlan can classify a
// registry outage; a dependency that cannot be fetched without running code
// is reported as an environment limitation, not a candidate failure.
func (p *Project) execDependencyPlan(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string) (*ExecResult, sandboxNetwork, error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	if _, err := userNetLookPath(userNetHelperName); err != nil {
		_, network := prepareUserNetwork(nil)
		result, err := p.execLoopbackStep(ctx, env, projectDir, plan, readOnly, true)
		return result, network, err
	}
	// A dependency that cannot be fetched without running project code (an
	// npm command line naming a package, a local-directory, workspace or git
	// dependency; a rustup toolchain chosen by path) must not run with the
	// network at all. It is an environment limitation of fetch-only
	// networking, not the candidate's failure.
	if reason := p.networkFetchUnavailable(plan); reason != "" {
		return refuseFetch(nil, none, reason)
	}
	if plan.Command == "npm" {
		return p.execNpmFetch(ctx, env, projectDir, plan, readOnly)
	}
	fetch, build, cleanup, err := p.planDependencyStages(plan, projectDir)
	var refusal *fetchRefusal
	if errors.As(err, &refusal) {
		return refuseFetch(nil, none, refusal.reason)
	}
	if err != nil {
		return nil, none, err
	}
	defer cleanup()

	var fetchResult *ExecResult
	fetchNetwork := none
	if fetch != nil {
		var attached bool
		fetchResult, fetchNetwork, attached, err = p.execStageFetch(ctx, env, projectDir, *fetch)
		if !attached && err == nil {
			// The network could not be set up: nothing ran with it. Run the
			// plan as a step without outbound network runs, which explains
			// the stage that failed if a download was needed.
			result, err := p.execLoopbackStep(ctx, env, projectDir, plan, readOnly, true)
			return result, fetchNetwork, err
		}
		if err != nil {
			return fetchResult, fetchNetwork, err
		}
		if fetchResult != nil && fetchResult.ExitCode != 0 {
			return p.classifyFailedFetch(ctx, env, plan, *fetch, fetchResult, fetchNetwork)
		}
		if err := writeBackFetchFiles(projectDir, fetch.stage); err != nil {
			return fetchResult, fetchNetwork, err
		}
	}
	if build == nil {
		return fetchResult, fetchNetwork, nil
	}

	// Read before the build runs project code that could rewrite the
	// declarations (see below).
	unfetched := p.pipUnfetchedRemoteTarget(plan)
	buildResult, err := p.execLoopbackStep(ctx, env, projectDir, *build, readOnly, false)
	if err != nil {
		return buildResult, none, err
	}
	// The build runs project code (setup.py, PEP 517 backends, build.rs)
	// loopback-only, so its output cannot be trusted to classify the failure: a
	// candidate can forge pip's own "No matching distribution" box. Everything
	// declared as a named requirement was fetched before the build, which
	// installs with --no-index, so a build failure is the candidate's own. It
	// is marked prefetched so RunVerificationPlan never reclassifies it as an
	// environment outage from the build's output.
	//
	// The one exception was established before any project code ran: a
	// declared VCS or URL dependency whose environment marker holds was never
	// fetched, so the offline install cannot obtain it, which the owner's
	// decision makes an environment limitation of fetch-only networking.
	if unfetched != "" && buildResult != nil && buildResult.ExitCode != 0 {
		msg := "the dependency " + unfetched + " is a VCS or URL reference, which makewand never downloads while the network is available (pip may build such a reference to read its metadata, running its code), so the offline install cannot obtain it - depend on a release from a package index, or install the project's dependencies outside makewand"
		buildResult.Note = msg
		return buildResult, none, fmt.Errorf("%w (%s)", ErrDependencyNetworkUnavailable, msg)
	}
	return buildResult, sandboxNetwork{kind: sandboxNetworkNone, prefetched: true}, nil
}

// execStageFetch runs the networked fetch of a split plan in its fetch
// sandbox, with the trusted executable for the plan's command.
func (p *Project) execStageFetch(ctx context.Context, env verifyExecEnvironment, projectDir string, fetch ExecPlan) (*ExecResult, sandboxNetwork, bool, error) {
	if fetch.Command == "go" {
		if err := checkSandboxGoToolchain(projectDir, verifyHomeLayout(projectDir, fetchPathEnv(projectDir), true).goVersion); err != nil {
			return nil, sandboxNetwork{kind: sandboxNetworkNone}, true, err
		}
	}
	spec := fetchSpec{
		workspace: projectDir,
		dir:       fetch.stage.dir.path,
		cache:     fetch.stage.area.cache,
		pathEnv:   fetchPathEnv(projectDir),
		env:       fetch.env,
	}
	command, args := fetchToolPath(fetch.Command, projectDir), fetch.Args
	guard := ""
	if fetch.Command == "pip" || fetch.Command == "pip3" {
		var why string
		command, args, guard, why = guardedPipFetch(projectDir, fetch.Command, fetch.Args)
		if why != "" {
			result, network, err := refuseFetch(nil, sandboxNetwork{kind: sandboxNetworkNone}, why)
			return result, network, true, err
		}
	}
	result, network, attached, err := p.execFetchCommand(ctx, env, spec, command, args, true)
	if err == nil && attached && result != nil && result.ExitCode != 0 && guard != "" &&
		(strings.Contains(result.Stdout, guard) || strings.Contains(result.Stderr, guard)) {
		result, network, err = refuseFetch(result, network, "pip dependency metadata requires a child process or the kernel execution guard could not be installed; makewand refuses to run build metadata while downloading, so install this dependency outside makewand or use index wheels with static metadata")
	}
	return result, network, attached, err
}

// classifyFailedFetch decides what a failed networked fetch means. A fetch
// runs no project code, so its output is the tool's own. A suspected registry
// outage is left to RunVerificationPlan's own connectivity check from the
// fetch's network. A requirement pip found no distribution for is an
// environment limitation only when the index shows sdist-only matching
// releases (pipReleaseCheck); otherwise the failure stays the candidate's.
func (p *Project) classifyFailedFetch(ctx context.Context, env verifyExecEnvironment, plan, fetch ExecPlan, result *ExecResult, network sandboxNetwork) (*ExecResult, sandboxNetwork, error) {
	if p.dependencyOutageSuspected(plan, result) || (plan.Command != "pip" && plan.Command != "pip3") {
		return result, network, nil
	}
	name := pipMissingDistribution(result)
	if name == "" {
		return result, network, nil
	}
	var markerEnv pipMarkerEnv
	if projectDir, err := p.execWorkingDir(); err == nil {
		markerEnv = pipMarkerEnvironment(pipFetchInterpreter(projectDir))
	}
	verdict, why := p.pipReleaseCheck(ctx, env, fetch.stage.pip, name, markerEnv)
	if verdict != pipReleaseSdistOnly {
		result.Note = why
		return result, network, nil
	}
	msg := p.unfetchableDependency(plan, result)
	result.Note = msg
	return result, network, fmt.Errorf("%w (%s)", ErrDependencyNetworkUnavailable, msg)
}

// writeBackFetchFiles puts the files a successful fetch produced that belong
// in the workspace there (writeBackWorkspaceFile).
func writeBackFetchFiles(projectDir string, stage *fetchStage) error {
	if stage == nil || len(stage.writeBack) == 0 {
		return nil
	}
	ws, err := os.OpenRoot(projectDir)
	if err != nil {
		return err
	}
	defer ws.Close()
	for _, rel := range stage.writeBack {
		data, err := stage.dir.readFile(rel, maxNpmManifestBytes)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read the %s the dependency fetch produced: %w", rel, err)
		}
		if err := writeBackWorkspaceFile(ws, rel, data); err != nil {
			return fmt.Errorf("put the %s the dependency fetch produced into the project: %w", rel, err)
		}
	}
	return nil
}

// planDependencyStages splits a dependency plan into a networked fetch that
// runs no project code and a loopback-only build/install that may. fetch is
// nil when the build runs loopback-only with nothing to fetch first; build is
// nil when the fetch is the whole step (it runs no project code). A pip, go
// or cargo fetch gets a fresh manifest-only fetch directory (fetch.stage) that
// cleanup removes. A plan makewand will not fetch with the network is a
// *fetchRefusal error.
func (p *Project) planDependencyStages(plan ExecPlan, projectDir string) (fetch, build *ExecPlan, cleanup func(), err error) {
	noop := func() {}
	if plan.Command == "npm" || plan.Command == "pnpm" || plan.Command == "yarn" {
		// install/ci fetch packages; --ignore-scripts keeps lifecycle scripts
		// from running, and npm_config_ignore_scripts=true closes the config a
		// .npmrc could set. execNpmFetch runs it in a fetch directory of its
		// own, with a lockfile it has inspected; npm_config_allow_git=none
		// makes npm 12 refuse a git dependency anywhere in the tree.
		f := plan
		f.Args = ensureArg(plan.Args, "--ignore-scripts")
		f.env = append(slices.Clone(plan.env), "npm_config_ignore_scripts=true", "npm_config_allow_git=none")
		return &f, nil, noop, nil
	}
	area, err := openDepFetchArea(projectDir)
	if err != nil {
		return nil, nil, noop, &fetchRefusal{reason: "makewand cannot prepare the networked dependency fetch outside the project (" + err.Error() + "); install the project's dependencies outside makewand"}
	}
	dir, err := area.newFetchDir()
	if err != nil {
		area.close()
		return nil, nil, noop, err
	}
	cleanup = func() {
		dir.remove()
		area.close()
	}
	ws, err := os.OpenRoot(projectDir)
	if err != nil {
		cleanup()
		return nil, nil, noop, err
	}
	defer ws.Close()
	stage := &fetchStage{area: area, dir: dir}
	switch plan.Command {
	case "go":
		fetch, err = p.goDependencyStages(plan, projectDir, ws, stage)
	case "cargo":
		fetch, build, err = p.cargoDependencyStages(plan, projectDir, ws, stage)
	case "pip", "pip3":
		fetch, build, err = p.pipDependencyStages(plan, ws, stage)
	default:
		// Any other dependency command (no plan makewand detects) runs as a
		// networked command in the empty fetch directory: it sees no file of
		// the project, so it cannot run one.
		f := plan
		fetch = &f
	}
	if err != nil {
		cleanup()
		return nil, nil, noop, err
	}
	if fetch != nil {
		fetch.stage = stage
	}
	return fetch, build, cleanup, nil
}

// cargoDependencyStages splits a cargo dependency plan: `cargo fetch` is the
// networked download, run on a copy of the workspace's manifests
// (cargoFetchSources); any build step (cargo build) then builds offline in the
// workspace from the fetched crates, so build.rs never runs with the network.
func (p *Project) cargoDependencyStages(plan ExecPlan, projectDir string, ws *os.Root, stage *fetchStage) (fetch, build *ExecPlan, err error) {
	if err := cargoFetchSources(ws, stage.dir); err != nil {
		return nil, nil, err
	}
	stage.writeBack = []string{"Cargo.lock"}
	fetchPlan := ExecPlan{Kind: "deps", Detector: plan.Detector, Command: "cargo", Args: []string{"fetch"}}
	if slices.Contains(plan.Args, "--locked") || slices.Contains(plan.Args, "--frozen") {
		fetchPlan.Args = append(fetchPlan.Args, "--locked")
	}
	if len(plan.Args) > 0 && plan.Args[0] == "fetch" {
		fetchPlan.Args = slices.Clone(plan.Args)
	} else {
		buildPlan := plan
		buildPlan.Args = ensureArg(plan.Args, "--offline")
		build = &buildPlan
	}
	fetchPlan.env = append(slices.Clone(plan.env), cargoFetchEnv(stage.area.cacheDir(fetchCacheCargo), projectDir)...)
	if pin, _ := p.cargoFetchToolchain(projectDir); pin != "" {
		// A toolchain the fetch cannot be pinned to refuses it with the
		// network (networkFetchUnavailable).
		fetchPlan.env = append(fetchPlan.env, "RUSTUP_TOOLCHAIN="+pin)
	}
	return &fetchPlan, build, nil
}

// cargoFetchEnv hardens the networked `cargo fetch` against the executables a
// cargo config can name and the fetch would otherwise run (confirmed on cargo
// 1.93): build.rustc, the rustc wrappers, and a registry credential provider.
// The fetch directory holds no .cargo directory, and CARGO_HOME is the shared
// cargo cache, whose config files only the host user writes. These settings
// add defence in depth (the rustup toolchain, which a workspace rust-toolchain
// file chooses, is pinned separately: cargoFetchToolchain):
//   - RUSTC is pinned to the trusted rustc of the fetch PATH (fetchToolPath),
//     so a config build.rustc is not consulted (on a rustup host that is the
//     rustc proxy, which follows the pinned toolchain);
//   - the three rustc-wrapper variables are cleared: an empty value overrides
//     a config rustc-wrapper or rustc-workspace-wrapper (verified on cargo
//     1.93);
//   - the global credential providers default to cargo's built-in token
//     provider;
//   - CARGO_HOME is the shared cargo cache (cargoHome), never the host
//     ~/.cargo, and is what the offline build reads read-only.
func cargoFetchEnv(cargoHome, projectDir string) []string {
	env := []string{
		"RUSTC_WRAPPER=",
		"RUSTC_WORKSPACE_WRAPPER=",
		"CARGO_BUILD_RUSTC_WRAPPER=",
		"CARGO_REGISTRY_GLOBAL_CREDENTIAL_PROVIDERS=cargo:token",
		"CARGO_HOME=" + cargoHome,
	}
	if rustc := fetchToolPath("rustc", projectDir); filepath.IsAbs(rustc) {
		env = append(env, "RUSTC="+rustc)
	}
	return env
}

// Limits of what makewand copies into a fetch directory.
const (
	maxFetchManifestBytes = 16 << 20
	maxFetchManifests     = 10000
)

// cargoFetchSkipDirs are directories the manifest walk never descends.
var cargoFetchSkipDirs = map[string]bool{"target": true, "node_modules": true, "vendor": true}

// cargoFetchSources copies every Cargo.toml and Cargo.lock of the workspace
// into the fetch directory, with an empty stub file at each target path cargo
// needs to load the manifests (src/lib.rs, src/main.rs, src/bin/*, a build
// script, explicit [lib], [[bin]], [[example]], [[test]] and [[bench]] paths)
// where the workspace has one - never its contents. Hidden directories, target,
// node_modules and vendor are not searched.
func cargoFetchSources(ws *os.Root, dir *fetchDir) error {
	manifests := 0
	return fs.WalkDir(ws.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == "." {
				return err
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != "." && (strings.HasPrefix(name, ".") || cargoFetchSkipDirs[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if name != "Cargo.toml" && name != "Cargo.lock" || !d.Type().IsRegular() {
			return nil
		}
		if manifests++; manifests > maxFetchManifests {
			return fmt.Errorf("the project holds more than %d Cargo manifests", maxFetchManifests)
		}
		rel := filepath.FromSlash(path)
		data, err := readRegularNoFollow(ws, rel, maxFetchManifestBytes)
		if err != nil {
			return err
		}
		if err := dir.writeFile(rel, data, 0o600); err != nil {
			return err
		}
		if name == "Cargo.toml" {
			return cargoTargetStubs(ws, dir, filepath.Dir(rel), string(data))
		}
		return nil
	})
}

// cargoTargetStubs writes the empty stub files of one manifest's targets.
func cargoTargetStubs(ws *os.Root, dir *fetchDir, manifestDir, manifest string) error {
	doc := parseTOML(manifest)
	candidates := []string{"src/lib.rs", "src/main.rs", "build.rs"}
	for _, entry := range doc.entries {
		key := entry.fullKey()
		switch {
		case key == "package.build", key == "lib.path",
			(entry.table == "bin" || entry.table == "example" || entry.table == "test" || entry.table == "bench") && entry.key == "path":
			if path := firstTOMLString(entry.value); path != "" {
				candidates = append(candidates, path)
			}
		}
	}
	if entries, err := fs.ReadDir(ws.FS(), filepath.ToSlash(filepath.Join(manifestDir, "src", "bin"))); err == nil {
		for _, entry := range entries {
			switch {
			case entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".rs"):
				candidates = append(candidates, "src/bin/"+entry.Name())
			case entry.IsDir():
				candidates = append(candidates, "src/bin/"+entry.Name()+"/main.rs")
			}
		}
	}
	for _, candidate := range candidates {
		rel := filepath.Clean(filepath.Join(manifestDir, filepath.FromSlash(candidate)))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if info, err := ws.Lstat(rel); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if _, err := dir.root.Lstat(rel); err == nil {
			continue // stubbed already
		}
		if err := dir.writeFile(rel, nil, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// goDependencyStages prepares the networked go fetch: `go mod tidy` or `go
// mod download` run as planned in a fetch directory holding the go.mod, go.sum
// and go.work files (goFetchSources), with the shared module cache, and go.mod
// and go.sum are put back into the workspace afterwards. The go command runs
// no project code; the offline steps read the fetched modules through GOPROXY
// (loopbackFetchCache).
func (p *Project) goDependencyStages(plan ExecPlan, projectDir string, ws *os.Root, stage *fetchStage) (*ExecPlan, error) {
	tidy := len(plan.Args) >= 2 && plan.Args[0] == "mod" && plan.Args[1] == "tidy"
	if err := p.goFetchSources(ws, projectDir, stage.dir, tidy); err != nil {
		return nil, err
	}
	stage.writeBack = []string{"go.mod", "go.sum"}
	f := plan
	// GOFLAGS=-modcacherw keeps the shared cache removable, as in every
	// sandbox (resolveSandboxToolchains).
	f.env = append(slices.Clone(plan.env), "GOMODCACHE="+stage.area.cacheDir(fetchCacheGo))
	return &f, nil
}

// goFetchSources copies into the fetch directory what the go command reads to
// resolve the dependencies: go.mod and go.sum, go.work and the go.mod and
// go.sum of each module it uses, and those of each module a replace directive
// points to in the workspace. For go mod tidy, which computes the requirements
// from the packages' imports, each Go file of those modules is copied as an
// import-only stub: its build constraints, package clause and import paths,
// never its code (goImportStub). A replacement outside the workspace refuses
// the networked fetch.
func (p *Project) goFetchSources(ws *os.Root, projectDir string, dir *fetchDir, tidy bool) error {
	modules := []string{"."}
	if data, err := readRegularNoFollow(ws, "go.work", maxFetchManifestBytes); err == nil {
		if err := dir.writeFile("go.work", data, 0o600); err != nil {
			return err
		}
		modules = append(modules, goWorkUses(string(data))...)
	}
	// Only the main modules' replace directives apply (the go command ignores
	// those of other modules): the module and the ones go.work uses.
	main := map[string]bool{}
	for _, module := range modules {
		main[filepath.Clean(filepath.FromSlash(module))] = true
	}
	seen := map[string]bool{}
	for len(modules) > 0 {
		module := filepath.Clean(filepath.FromSlash(modules[0]))
		modules = modules[1:]
		if seen[module] {
			continue
		}
		seen[module] = true
		if filepath.IsAbs(module) || module == ".." || strings.HasPrefix(module, ".."+string(filepath.Separator)) {
			return &fetchRefusal{reason: "the project's Go workspace uses the module directory " + truncateForNote(module) + ", which lies outside the project and so outside what the networked go fetch sees; install the project's dependencies outside makewand"}
		}
		gomod, err := readRegularNoFollow(ws, filepath.Join(module, "go.mod"), maxFetchManifestBytes)
		if err != nil {
			if module == "." && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if err := dir.writeFile(filepath.Join(module, "go.mod"), gomod, 0o600); err != nil {
			return err
		}
		if sum, err := readRegularNoFollow(ws, filepath.Join(module, "go.sum"), maxFetchManifestBytes); err == nil {
			if err := dir.writeFile(filepath.Join(module, "go.sum"), sum, 0o600); err != nil {
				return err
			}
		}
		if main[module] {
			replacements, err := p.goLocalReplacements(projectDir, module, gomod)
			if err != nil {
				return err
			}
			modules = append(modules, replacements...)
		}
		if tidy {
			if err := goImportStubs(ws, dir, module); err != nil {
				return err
			}
		}
	}
	return nil
}

// goWorkUses returns the directories a go.work file's use directives name.
func goWorkUses(content string) []string {
	var uses []string
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		switch {
		case inBlock && len(fields) > 0 && fields[0] == ")":
			inBlock = false
		case inBlock && len(fields) > 0:
			uses = append(uses, strings.Trim(fields[0], "\"`"))
		case len(fields) >= 2 && fields[0] == "use" && fields[1] == "(":
			inBlock = true
		case len(fields) >= 2 && fields[0] == "use":
			uses = append(uses, strings.Trim(fields[1], "\"`"))
		}
	}
	return uses
}

// goLocalReplacements returns the workspace-relative directories the replace
// directives of a module's go.mod point to. One outside the workspace is a
// *fetchRefusal.
func (p *Project) goLocalReplacements(projectDir, module string, gomod []byte) ([]string, error) {
	if !strings.Contains(string(gomod), "replace") {
		return nil, nil
	}
	pathEnv := fetchPathEnv(projectDir)
	goBin := trustedToolPath("go", projectDir, pathEnv)
	if !filepath.IsAbs(goBin) {
		return nil, nil // no go command: the fetch fails as a missing tool does
	}
	mod, err := goModParse(goBin, pathEnv, gomod)
	if err != nil {
		return nil, nil // the go command reports the broken go.mod itself
	}
	var dirs []string
	for _, rep := range mod.Replace {
		if rep.New.Version != "" {
			continue
		}
		target := filepath.FromSlash(rep.New.Path)
		if !filepath.IsAbs(target) {
			target = filepath.Join(projectDir, module, target)
		}
		rel, err := filepath.Rel(projectDir, filepath.Clean(target))
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, &fetchRefusal{reason: "go.mod replaces " + truncateForNote(rep.Old.Path) + " with the directory " + truncateForNote(rep.New.Path) + ", which lies outside the project and so outside what the networked go fetch sees; install the project's dependencies outside makewand"}
		}
		dirs = append(dirs, rel)
	}
	return dirs, nil
}

// goImportStubs writes an import-only stub (goImportStub) of each Go file of
// the module in dir that `go mod tidy` loads: outside directories the go
// command skips (.foo, _foo, testdata, vendor, nested modules).
func goImportStubs(ws *os.Root, dst *fetchDir, module string) error {
	files := 0
	return fs.WalkDir(ws.FS(), filepath.ToSlash(module), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == filepath.ToSlash(module) {
				return err
			}
			return nil
		}
		name := d.Name()
		rel := filepath.FromSlash(path)
		if d.IsDir() {
			if rel == module {
				return nil
			}
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" {
				return fs.SkipDir
			}
			if info, err := ws.Lstat(filepath.Join(rel, "go.mod")); err == nil && info.Mode().IsRegular() {
				return fs.SkipDir // a nested module
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		if files++; files > goImportScanLimit {
			return &fetchRefusal{reason: "the project holds more than " + strconv.Itoa(goImportScanLimit) + " Go files, more than makewand prepares for the networked go mod tidy; install the project's dependencies outside makewand"}
		}
		src, err := readRegularNoFollow(ws, rel, goImportScanFileBytes)
		if err != nil {
			return &fetchRefusal{reason: "makewand cannot read " + rel + " to prepare the networked go mod tidy (" + err.Error() + "); install the project's dependencies outside makewand"}
		}
		return dst.writeFile(rel, goImportStub(src), 0o600)
	})
}

// goImportStub returns what `go mod tidy` reads of a Go file: its build
// constraint lines, its package clause and its import paths, with no code. A
// file whose header does not parse becomes a stub that does not parse either,
// so tidy fails on it as it would on the original.
func goImportStub(src []byte) []byte {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return []byte("// makewand: the original file does not parse\npackage\n")
	}
	var b strings.Builder
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, c := range group.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				b.WriteString(c.Text + "\n")
			}
		}
	}
	b.WriteString("\npackage " + file.Name.Name + "\n")
	for _, spec := range file.Imports {
		if spec.Path.Value == `"C"` {
			b.WriteString("\nimport \"C\"\n") // cgo's pseudo-package takes no name
			continue
		}
		b.WriteString("\nimport _ " + spec.Path.Value + "\n")
	}
	return []byte(b.String())
}
