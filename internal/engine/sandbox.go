package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/version"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
)

// ExecResult holds the result of a command execution.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	// Dropped bytes report output discarded after the combined raw stream cap.
	// Such results carry an uncertain-outcome error and must not count as passed.
	StdoutDroppedBytes int64
	StderrDroppedBytes int64
	// Note is makewand's own remark about the run, never command output: why
	// output that suggested a registry outage still left a failed dependency
	// install the command's own failure (dependencyRegistryError).
	Note string
}

// ExecPlan describes one command candidate detected from project files.
type ExecPlan struct {
	Kind     string // "deps" or "tests"
	Detector string // which file triggered this plan
	Command  string
	Args     []string

	// env holds KEY=VALUE settings added to the command's environment. Only
	// verification's own adaptations of a plan set it.
	env []string
}

// DisplayCommand renders a shell-like command summary for user confirmation.
func (p ExecPlan) DisplayCommand() string {
	var parts []string
	parts = append(parts, quoteArg(p.Command))
	for _, arg := range p.Args {
		parts = append(parts, quoteArg(arg))
	}
	return strings.Join(parts, " ")
}

type execPolicy struct {
	allowAnyCommand bool
	stripSensitive  bool
	allowedCommands map[string]struct{}
	// allowCommandPath permits an absolute wrapper binary (bubblewrap). The
	// wrapped inner command must already be validated against the restricted
	// allowlist before this policy is used.
	allowCommandPath bool
	// sandboxed applies the processjob sandbox limits (memory cap) and, for a
	// bwrap argv led by `--seccomp 3`, supplies the syscall filter descriptor.
	sandboxed bool
	// env holds KEY=VALUE settings added to the command's environment.
	env []string
}

var restrictedAllowedCommands = map[string]struct{}{
	"npm":     {},
	"node":    {},
	"pnpm":    {},
	"yarn":    {},
	"pip":     {},
	"pip3":    {},
	"python":  {},
	"python3": {},
	"go":      {},
	"cargo":   {},
	"pytest":  {},
}

var sensitiveEnvExact = map[string]struct{}{
	"ANTHROPIC_API_KEY":     {},
	"GEMINI_API_KEY":        {},
	"OPENAI_API_KEY":        {},
	"AWS_ACCESS_KEY_ID":     {},
	"AWS_SECRET_ACCESS_KEY": {},
	"AWS_SESSION_TOKEN":     {},
	"GITHUB_TOKEN":          {},
	"GH_TOKEN":              {},
	"NPM_TOKEN":             {},
}

var sensitiveEnvSuffixes = []string{
	"_API_KEY",
	"_TOKEN",
	"_SECRET",
	"_PASSWORD",
}

func trustedExecPolicy() execPolicy {
	return execPolicy{
		allowAnyCommand: true,
		stripSensitive:  false,
	}
}

func restrictedExecPolicy() execPolicy {
	return execPolicy{
		allowAnyCommand: false,
		stripSensitive:  true,
		allowedCommands: restrictedAllowedCommands,
		sandboxed:       true,
	}
}

func isolatedExecPolicy() execPolicy {
	return execPolicy{
		// The bwrap wrapper is invoked by absolute path; the inner command was
		// already validated against restrictedAllowedCommands before wrapping.
		allowAnyCommand:  true,
		stripSensitive:   true, // defense in depth; bwrap --clearenv rebuilds the env anyway
		allowCommandPath: true,
		sandboxed:        true,
	}
}

// Exec runs a trusted command in the project directory with a timeout.
// Used by internal engine flows like git operations.
func (p *Project) Exec(ctx context.Context, command string, args ...string) (*ExecResult, error) {
	return p.execWithPolicy(ctx, command, args, trustedExecPolicy())
}

// ExecRestricted runs a restricted command in the project directory.
// Command name must be allowlisted and sensitive environment variables are stripped.
func (p *Project) ExecRestricted(ctx context.Context, command string, args ...string) (*ExecResult, error) {
	return p.execWithPolicy(ctx, command, args, restrictedExecPolicy())
}

// RunRestrictedPlan executes a detected restricted plan for the real project
// (dependency installs, test runs, and auto-fix retries). It applies the SAME
// fail-closed isolation gate as candidate verification: when bubblewrap
// isolation is available the command runs inside the sandbox (network-isolated
// unless it is a dependency install); when isolation is unavailable no command
// runs on the host unless the user explicitly opts in with
// MAKEWAND_UNSAFE_HOST_EXEC=1. Otherwise a clear "isolation unavailable" error
// is returned so callers (the TUI) degrade to manual approval instead of
// silently executing generated commands directly on the host.
func (p *Project) RunRestrictedPlan(ctx context.Context, plan ExecPlan) (*ExecResult, error) {
	return p.RunVerificationPlan(ctx, plan)
}

// RunVerificationPlan executes a candidate verification plan and fails closed:
// without working bubblewrap isolation (or the explicit
// MAKEWAND_UNSAFE_HOST_EXEC=1 opt-in) no command is executed at all.
// Every step runs in its own network namespace. Dependency installs get
// outbound-only access through slirp4netns when it is installed (see
// sandbox_net.go); a dependency install that fails because its registry is
// unreachable (dependencyRegistryError) returns its result together with an
// error wrapping ErrDependencyNetworkUnavailable, so callers treat it as an
// environment problem rather than a candidate failure.
//
// Protected paths (isProtectedWritePath) are guarded around every run: they are
// bound read-only inside the sandbox, and any change that still happens (or
// happens on the acknowledged unsafe host path) is rolled back and reported as
// an error wrapping ErrProtectedPathsModified, alongside the command result.
func (p *Project) RunVerificationPlan(ctx context.Context, plan ExecPlan) (*ExecResult, error) {
	env, err := resolveVerifyExecEnvironment(p.unsafeHostAuth)
	if err != nil {
		return nil, err
	}
	projectDir, err := p.execWorkingDir()
	if err != nil {
		return nil, err
	}
	guard, err := snapshotProtectedPaths(projectDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot protected paths before running %s: %w", plan.Command, err)
	}

	var result *ExecResult
	var network sandboxNetwork
	if env.mode == verifyExecUnsafeHost {
		// Audit EVERY host execution under the unsafe opt-in, not just the
		// first: the acknowledgment authorizes the mode, the audit trail
		// records each use of it.
		p.unsafeHostAuth.audit(UnsafeHostExecEvent{
			Context: "verification",
			Command: plan.Command,
			Args:    append([]string(nil), plan.Args...),
			Dir:     p.Path,
		})
		policy := restrictedExecPolicy()
		policy.env = plan.env
		result, err = p.execWithPolicy(ctx, plan.Command, plan.Args, policy)
		network = sandboxNetwork{kind: sandboxNetworkHost}
	} else {
		result, network, err = p.execVerification(ctx, plan, guard.readOnlyBinds())
	}
	if err == nil {
		var note string
		note, err = p.dependencyRegistryError(ctx, env, plan, result, network)
		if note != "" && result != nil {
			result.Note = note
		}
	}
	if guardErr := guard.enforce(); guardErr != nil {
		if err != nil {
			return result, fmt.Errorf("%w; %v", guardErr, err)
		}
		return result, guardErr
	}
	return result, err
}

// verificationPlanAllowsNetwork reports whether a plan may share the HOST
// network namespace. None may: every verification plan, dependency installs
// included, runs with --unshare-net so code executed by build steps (setup.py,
// build.rs) cannot reach host loopback services or abstract AF_UNIX sockets.
// Bubblewrap brings up loopback in the isolated namespace, so local mock
// servers work. Dependency installs additionally get outbound-only access
// through a user-mode network stack (verificationPlanUsesUserNetwork).
func verificationPlanAllowsNetwork(plan ExecPlan) bool {
	return false
}

// execVerification runs plan inside the bubblewrap sandbox. A dependency plan
// is split into a networked fetch that runs no project code and a
// loopback-only build/install that may (execDependencyPlan); every other step
// runs loopback-only. It also returns the network the command had, for a
// dependency install whose registry turns out to be unreachable.
func (p *Project) execVerification(ctx context.Context, plan ExecPlan, readOnly []string) (*ExecResult, sandboxNetwork, error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	if err := validateCommandName(plan.Command); err != nil {
		return nil, none, err
	}
	if _, ok := restrictedAllowedCommands[plan.Command]; !ok {
		return nil, none, fmt.Errorf("command %q is blocked by execution policy", plan.Command)
	}

	env, err := resolveVerifyExecEnvironment(p.unsafeHostAuth)
	if err != nil {
		return nil, none, err
	}
	if env.mode != verifyExecIsolated {
		return nil, none, fmt.Errorf("verification sandbox is not active")
	}

	projectDir, err := p.execWorkingDir()
	if err != nil {
		return nil, none, err
	}
	if err := p.prepareSandboxHome(projectDir); err != nil {
		return nil, none, err
	}

	if verificationPlanUsesUserNetwork(plan) {
		return p.execDependencyPlan(ctx, env, projectDir, plan, readOnly)
	}
	return p.execVerificationStep(ctx, env, projectDir, plan, readOnly, false)
}

// prepareSandboxHome creates the workspace-relative sandbox HOME and turns Go
// telemetry off there. The sandbox HOME lives inside the writable workspace so
// a dependency fetch can populate tool caches that network-isolated steps
// reuse.
func (p *Project) prepareSandboxHome(projectDir string) error {
	home := filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create sandbox home: %w", err)
	}
	return disableGoTelemetry(home)
}

// disableGoTelemetry writes the mode file `go telemetry off` writes (verified
// against go1.27) into a sandbox HOME. In its default "local" mode a sandboxed
// go command collects counters and can spawn an upload child that writes under
// <home>/.config/go/telemetry after the step returns; such a late write races
// t.TempDir's RemoveAll and reddens otherwise-green runs. "off" stops the
// collection, so no telemetry or upload.token files are ever created.
func disableGoTelemetry(sandboxHome string) error {
	dir := filepath.Join(sandboxHome, ".config", "go", "telemetry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("disable go telemetry for the sandbox: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("off\n"), 0o600); err != nil {
		return fmt.Errorf("disable go telemetry for the sandbox: %w", err)
	}
	return nil
}

// execVerificationStep runs one verification command in the sandbox. A
// networked step is a dependency fetch: it gets outbound user-mode network and
// runs without the host module caches or workspace secrets (it executes no
// project code). Every other step runs loopback-only, with the host caches
// available (it may execute project code but has no network to exfiltrate
// anything).
func (p *Project) execVerificationStep(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string, networked bool) (*ExecResult, sandboxNetwork, error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	if networked {
		return p.execNetworkedFetch(ctx, env, projectDir, plan, readOnly)
	}
	wrappedCmd, wrappedArgs, err := p.wrapSandboxStep(ctx, env, projectDir, plan, readOnly, false, false)
	if err != nil {
		return nil, none, err
	}
	result, err := p.execSandboxed(ctx, wrappedCmd, wrappedArgs, nil)
	return result, none, err
}

// wrapSandboxStep builds the bwrap argv for one plan and applies the per-run
// settings (acceptance isolation, plan environment) and the go toolchain
// precondition. noHostModuleCache and maskWorkspaceSecrets harden a networked
// fetch (see wrapVerificationCommand).
func (p *Project) wrapSandboxStep(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string, noHostModuleCache, maskWorkspaceSecrets bool) (string, []string, error) {
	wrappedCmd, wrappedArgs, err := wrapVerificationCommand(env.bwrapPath, projectDir, plan.Command, plan.Args, verificationPlanAllowsNetwork(plan), readOnly, noHostModuleCache, maskWorkspaceSecrets)
	if err != nil {
		return "", nil, fmt.Errorf("verification sandbox: %w", err)
	}
	if acceptancePolicy(ctx) != nil {
		// Local diagnostics must not introduce undeclared cache files into the
		// execution tree later sealed by independent acceptance.
		settings := []string{"--setenv", "PYTHONDONTWRITEBYTECODE", "1", "--setenv", "PYTHONPYCACHEPREFIX", "/tmp/makewand-bytecode", "--setenv", "PYTEST_ADDOPTS", "-p no:cacheprovider", "--setenv", "CARGO_TARGET_DIR", filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath), "cargo-target")}
		wrappedArgs = insertBeforeCommand(wrappedArgs, len(plan.Args), settings...)
	}
	for _, setting := range plan.env {
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

// execNetworkedFetch runs a dependency fetch with outbound user-mode network.
// The fetch runs no project code, but is still built without the host module
// caches or workspace secrets. When the network helper is absent or cannot
// attach, the step runs loopback-only instead - and then with the host caches,
// which a step without outbound network may read safely.
func (p *Project) execNetworkedFetch(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string) (*ExecResult, sandboxNetwork, error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	networkedCmd, networkedArgs, err := p.wrapSandboxStep(ctx, env, projectDir, plan, readOnly, true, true)
	if err != nil {
		return nil, none, err
	}
	userNet, network := prepareUserNetwork(networkedArgs)
	if userNet == nil {
		return p.execLoopbackFetch(ctx, env, projectDir, plan, readOnly, network)
	}
	defer userNet.release()
	result, err := p.execSandboxed(ctx, networkedCmd, insertBeforeCommand(networkedArgs, len(plan.Args), userNet.bwrapArgs()...), userNet)
	if err != nil || userNet.attached {
		return result, network, err
	}
	// The helper is installed but could not be attached, so the sandbox was
	// stopped instead of running the fetch without the network it was meant to
	// have (execWithPolicyLaunch). Run it again the way a fetch without the
	// helper runs: one that needs no download still succeeds, and one that does
	// fails fast and reports the stage that could not be set up.
	return p.execLoopbackFetch(ctx, env, projectDir, plan, readOnly, userNet.network())
}

// execLoopbackFetch runs a dependency fetch loopback-only, with the host caches
// available (no outbound network to exfiltrate them) and npm's registry
// retries disabled so a needed download fails fast.
func (p *Project) execLoopbackFetch(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string, network sandboxNetwork) (*ExecResult, sandboxNetwork, error) {
	wrappedCmd, wrappedArgs, err := p.wrapSandboxStep(ctx, env, projectDir, plan, readOnly, false, false)
	if err != nil {
		return nil, network, err
	}
	offlineArgs := insertBeforeCommand(wrappedArgs, len(plan.Args), offlineDependencySettings()...)
	result, err := p.execSandboxed(ctx, wrappedCmd, offlineArgs, nil)
	return result, network, err
}

// execDependencyPlan runs a dependency plan as the owner's 2026-10-21 decision
// requires: only downloading uses the network. A networked fetch that runs no
// project code (pip download of wheels only, npm --ignore-scripts, cargo fetch,
// go mod download/tidy) fills the sandbox's own caches; a loopback-only build
// then installs from them, running any project code (setup.py, build.rs) with
// no network. The failed step's result and the fetch's network are returned so
// RunVerificationPlan can classify a registry outage; a dependency that cannot
// be fetched without running code is reported as an environment limitation,
// not a candidate failure.
func (p *Project) execDependencyPlan(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string) (*ExecResult, sandboxNetwork, error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	fetch, build, cleanup, err := p.planDependencyStages(plan, projectDir)
	if err != nil {
		return nil, none, err
	}
	defer cleanup()

	// A dependency that cannot be fetched without running project code (an npm
	// local-directory or git dependency, whose `prepare` runs even under
	// --ignore-scripts on some npm versions) must not run with the network at
	// all. It is an environment limitation of fetch-only networking, not the
	// candidate's failure.
	if reason := p.networkFetchUnavailable(plan); reason != "" {
		result := &ExecResult{ExitCode: 1, Note: reason}
		return result, none, fmt.Errorf("%w (%s)", ErrDependencyNetworkUnavailable, reason)
	}

	var fetchResult *ExecResult
	fetchNetwork := none
	if fetch != nil {
		fetchResult, fetchNetwork, err = p.execVerificationStep(ctx, env, projectDir, *fetch, readOnly, true)
		if err != nil {
			return fetchResult, fetchNetwork, err
		}
		if fetchResult != nil && fetchResult.ExitCode != 0 {
			// A fetch that looks like a registry outage is classified by
			// RunVerificationPlan's own connectivity check from the fetch's
			// network. The fetch runs no project code, so its output is pip's
			// own and can be trusted here.
			if !p.dependencyOutageSuspected(plan, fetchResult) {
				if name := pipMissingDistribution(fetchResult); name != "" {
					// A requirement the default index does not have at all is the
					// candidate's own broken dependency; one that exists only as a
					// source distribution is an environment limitation of
					// fetch-only networking (makewand downloads wheels only). When
					// the index is custom or the check cannot run, stay
					// conservative and call it environment.
					if p.pipProjectAbsent(plan, name) {
						fetchResult.Note = "the requirement " + name + " does not exist on the package index, so this is the candidate's own broken dependency"
						return fetchResult, fetchNetwork, nil
					}
					msg := p.unfetchableDependency(plan, fetchResult)
					fetchResult.Note = msg
					return fetchResult, fetchNetwork, fmt.Errorf("%w (%s)", ErrDependencyNetworkUnavailable, msg)
				}
			}
			return fetchResult, fetchNetwork, nil
		}
	}

	if build == nil {
		return fetchResult, fetchNetwork, nil
	}

	buildResult, _, err := p.execVerificationStep(ctx, env, projectDir, *build, readOnly, false)
	if err != nil {
		return buildResult, none, err
	}
	// The build runs project code (setup.py, PEP 517 backends, build.rs)
	// loopback-only, so its output cannot be trusted to classify the failure: a
	// candidate can forge pip's own "No matching distribution" box. Everything
	// declared (named requirements, build-system requires, static dependencies)
	// was fetched into the wheelhouse before the build, which installs with
	// --no-index, so a build failure is the candidate's own. It is marked
	// prefetched so RunVerificationPlan never reclassifies it as an environment
	// outage from the build's output or from the now-stale offlineRegistryNeed.
	return buildResult, sandboxNetwork{kind: sandboxNetworkNone, prefetched: true}, nil
}

// planDependencyStages splits a dependency plan into a networked fetch that
// runs no project code and a loopback-only build/install that may. fetch is
// nil when the build runs loopback-only with nothing to fetch first; build is
// nil when the fetch is the whole step (it runs no project code). cleanup
// removes any sandbox-private wheelhouse the fetch downloads into.
func (p *Project) planDependencyStages(plan ExecPlan, projectDir string) (fetch, build *ExecPlan, cleanup func(), err error) {
	noop := func() {}
	switch plan.Command {
	case "npm", "pnpm", "yarn":
		// install/ci fetch packages; --ignore-scripts keeps lifecycle scripts
		// from running, and npm_config_ignore_scripts=true closes the config a
		// workspace .npmrc could set. A local-directory or git dependency still
		// runs its `prepare` under --ignore-scripts on some npm versions
		// (GHSA-wr8v-3jqh-9x36), so execDependencyPlan refuses the networked
		// fetch outright when the manifest or lockfile names a non-registry
		// dependency (npmFetchUnavailable); here the fetch is registry-only.
		f := plan
		f.Args = ensureArg(plan.Args, "--ignore-scripts")
		f.env = append(slices.Clone(plan.env), "npm_config_ignore_scripts=true")
		return &f, nil, noop, nil
	case "go":
		// go mod tidy/download run no project code; the offline test step reuses
		// the sandbox module cache they fill.
		f := plan
		return &f, nil, noop, nil
	case "cargo":
		fetch, build = cargoDependencyStages(plan)
		fetch.env = append(slices.Clone(fetch.env), cargoFetchEnv(projectDir)...)
		return fetch, build, noop, nil
	case "pip", "pip3":
		return p.pipDependencyStages(plan, projectDir)
	}
	// Any other dependency command runs as a single networked fetch. Only pip
	// and cargo build execute project code while installing, and both are split
	// above; no other dependency plan makewand produces does, so there is
	// nothing to run loopback-only. A step that does get the network is still
	// confined to public destinations (sandbox_net.go).
	f := plan
	return &f, nil, noop, nil
}

// cargoDependencyStages splits a cargo dependency plan: `cargo fetch` is the
// networked download; any build step (cargo build) fetches first, then builds
// offline from the fetched crates (so build.rs never runs with the network).
func cargoDependencyStages(plan ExecPlan) (fetch, build *ExecPlan) {
	if len(plan.Args) > 0 && plan.Args[0] == "fetch" {
		f := plan
		return &f, nil
	}
	fetchPlan := ExecPlan{Kind: "deps", Detector: plan.Detector, Command: "cargo", Args: []string{"fetch"}}
	if slices.Contains(plan.Args, "--locked") || slices.Contains(plan.Args, "--frozen") {
		fetchPlan.Args = append(fetchPlan.Args, "--locked")
	}
	buildPlan := plan
	buildPlan.Args = ensureArg(plan.Args, "--offline")
	return &fetchPlan, &buildPlan
}

// cargoFetchEnv hardens the networked `cargo fetch` against the executables a
// cargo config can name and the fetch would otherwise run (confirmed on cargo
// 1.93): build.rustc, the rustc wrappers, and a registry credential provider.
// The config files that could name them are masked for the fetch
// (sandboxWorkspaceSecretMasks): every workspace .cargo/ directory and the
// config files of the sandbox CARGO_HOME. These settings cover what is left:
//   - RUSTC is pinned to the rustc resolved from the sandbox PATH, so a config
//     build.rustc is not consulted;
//   - the three rustc-wrapper variables are cleared: an empty value overrides
//     a config rustc-wrapper or rustc-workspace-wrapper (verified on cargo
//     1.93);
//   - the global credential providers default to cargo's built-in token
//     provider. This alone does not stop a provider a config file names (it
//     still runs, verified on cargo 1.93), which is why those files are masked;
//   - CARGO_HOME is the sandbox's own, never the host ~/.cargo (its
//     credentials and config), and is the same home the offline build reuses.
func cargoFetchEnv(projectDir string) []string {
	home := filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath))
	env := []string{
		"RUSTC_WRAPPER=",
		"RUSTC_WORKSPACE_WRAPPER=",
		"CARGO_BUILD_RUSTC_WRAPPER=",
		"CARGO_REGISTRY_GLOBAL_CREDENTIAL_PROVIDERS=cargo:token",
		"CARGO_HOME=" + filepath.Join(home, ".cargo"),
	}
	if rustc := lookPathIn("rustc", sandboxPathEnv()); rustc != "" {
		env = append(env, "RUSTC="+rustc)
	}
	return env
}

// pipDependencyStages splits a `pip install` into a `pip download` of wheels
// only (no sdist builds, so no project code runs with the network) into a
// sandbox-private wheelhouse, then an offline `pip install --no-index
// --find-links <wheelhouse>` that builds any local project and installs from
// what was fetched.
func (p *Project) pipDependencyStages(plan ExecPlan, projectDir string) (fetch, build *ExecPlan, cleanup func(), err error) {
	noop := func() {}
	install, ok := p.parsePipInstall(plan.Args)
	if !ok {
		b := plan
		return nil, &b, noop, nil
	}
	home := filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath))
	wheelhouse := filepath.Join(home, "makewand-wheelhouse")
	if err := os.MkdirAll(wheelhouse, 0o700); err != nil {
		return nil, nil, noop, fmt.Errorf("create dependency wheelhouse: %w", err)
	}
	constraints := filepath.Join(home, "makewand-fetch-constraints.txt")
	cleanup = func() { _ = os.RemoveAll(wheelhouse); _ = os.Remove(constraints) }
	buildPlan := plan
	buildPlan.Args = pipOfflineInstallArgs(plan.Args, wheelhouse)
	fetchArgs, hasTargets := p.pipDownloadArgs(install, wheelhouse)
	if !hasTargets {
		// Nothing the install resolves from a package index (for example `-e .`
		// with no declared dependencies): there is nothing to download, so no
		// networked step runs at all - the offline build installs the local
		// project from source loopback-only.
		return nil, &buildPlan, cleanup, nil
	}
	// Pin the fetch to the install's constraints, sanitised to named pins only,
	// so the wheels downloaded are the versions the offline install resolves.
	if len(install.constraintPins) > 0 {
		if err := os.WriteFile(constraints, []byte(strings.Join(install.constraintPins, "\n")+"\n"), 0o600); err != nil {
			return nil, nil, cleanup, fmt.Errorf("write sanitised pip constraints: %w", err)
		}
		fetchArgs = append(fetchArgs, "--constraint", constraints)
	}
	fetchPlan := ExecPlan{Kind: "deps", Detector: plan.Detector, Command: plan.Command, Args: fetchArgs}
	return &fetchPlan, &buildPlan, cleanup, nil
}

// pipDownloadArgs builds the `pip download` command line for a parsed install:
// wheels only (--only-binary=:all:) into the wheelhouse. It passes only the
// named index requirements the install resolves - those from the command line
// and from the requirement files makewand can read, and, for each local project
// the install builds, its build-system requires and its statically declared
// dependencies (what a setup.py computes at build time cannot be known without
// running it). It never passes a requirement file (-r) or constraint file (-c),
// nor any buildable target (a local project, an sdist, a VCS reference, a URL,
// or a name @ url direct reference): pip builds such a target to read its
// metadata even under --only-binary=:all: (confirmed on pip 24 and 26), which
// would run project code while the network is available. The offline build
// installs the local project from the wheelhouse instead, loopback-only.
// hasTargets reports whether anything resolves from an index at all.
func (p *Project) pipDownloadArgs(install pipInstall, wheelhouse string) (args []string, hasTargets bool) {
	var reqs []string
	add := func(texts ...string) {
		for _, text := range texts {
			if named, ok := pipNamedRequirementText(text); ok {
				reqs = append(reqs, named)
			}
		}
	}
	add(install.downloadReqs...)
	for _, dir := range install.projects {
		add(p.pipBuildSystemRequires(dir)...)
		for _, dep := range p.pipStaticDependencies(dir) {
			add(dep.text)
		}
	}
	if len(reqs) == 0 {
		return nil, false
	}
	args = []string{"download", "--only-binary=:all:", "--no-input", "--dest", wheelhouse}
	args = append(args, install.indexArgs...) // sanitised http(s) indexes only
	args = append(args, reqs...)
	return args, true
}

// pipBuildSystemRequires returns the build-system requirements pip installs
// into an isolated build environment for the local project in dir: the
// pyproject.toml [build-system] requires, or the PEP 518 default
// (setuptools and wheel) when a pyproject.toml has no [build-system] table or
// the project builds from a setup.py. The offline install needs these wheels
// in the wheelhouse to build the project without the network.
func (p *Project) pipBuildSystemRequires(dir string) []string {
	if content, err := p.ReadFile(filepath.Join(dir, "pyproject.toml")); err == nil {
		doc := parseTOML(content)
		if value, ok := doc.value("build-system", "requires"); ok {
			return tomlStrings(value)
		}
		if doc.hasTable("build-system") {
			return nil // pip reports a [build-system] without requires during the build
		}
		return []string{"setuptools>=40.8.0", "wheel"}
	}
	if _, err := p.ReadFile(filepath.Join(dir, "setup.py")); err == nil {
		return []string{"setuptools>=40.8.0", "wheel"}
	}
	return nil
}

// pipOfflineInstallArgs turns a `pip install` command line into one that
// installs from the wheelhouse only, keeping its other arguments (--user,
// -e ., -r, --use-pep517). Build isolation then takes its build requirements
// from the wheelhouse too.
func pipOfflineInstallArgs(origArgs []string, wheelhouse string) []string {
	out := make([]string, 0, len(origArgs)+3)
	out = append(out, origArgs[0]) // "install"
	out = append(out, "--no-index", "--find-links", wheelhouse)
	return append(out, origArgs[1:]...)
}

// ensureArg appends flag to args unless it is already present.
func ensureArg(args []string, flag string) []string {
	if slices.Contains(args, flag) {
		return args
	}
	return append(slices.Clone(args), flag)
}

// dependencyOutageSuspected reports whether a failed dependency step printed a
// package manager diagnostic that a registry could not be reached. It only
// raises the suspicion; RunVerificationPlan's own connectivity check decides
// the verdict.
func (p *Project) dependencyOutageSuspected(plan ExecPlan, result *ExecResult) bool {
	return p.registryFailureEvidence(plan, result) != ""
}

// unfetchableDependency reports, for a failed pip step, that a required
// distribution could not be obtained as a wheel: the networked fetch downloads
// wheels only so no project code runs with the network, and the offline
// install reads only what was fetched. A source-only (sdist) distribution, or
// one pip would build from a repository, therefore cannot be installed. It is
// an environment limitation of fetch-only networking, not the candidate's
// failure. "" when no such distribution is reported.
func (p *Project) unfetchableDependency(plan ExecPlan, result *ExecResult) string {
	if plan.Command != "pip" && plan.Command != "pip3" {
		return ""
	}
	if name := pipMissingDistribution(result); name != "" {
		return "no installable wheel for " + name + "; makewand downloads wheels only so no project code runs while the network is available, so a source-only or repository-built dependency cannot be installed - provide it as a wheel or install the project's dependencies outside makewand"
	}
	return ""
}

// pipProjectAbsent reports whether the requirement name the fetch could not
// find does not exist on the package index at all (a candidate's own broken
// dependency), as opposed to existing only as a source distribution the
// wheel-only fetch cannot use (an environment limitation). It answers only
// when the fetch used the default PyPI index, with a code-free fact check of
// the index (pypiProjectExists); a custom index, or a check that cannot run,
// leaves the conservative environment verdict.
func (p *Project) pipProjectAbsent(plan ExecPlan, name string) bool {
	if plan.Command != "pip" && plan.Command != "pip3" {
		return false
	}
	install, ok := p.parsePipInstall(plan.Args)
	if !ok || install.customIndex || p.pipConfigMayAvoidIndex() || p.pipConfigSetsIndex() {
		return false
	}
	m := pipRequirementRe.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	return pypiProjectExists(normalizePipName(m[1])) == pypiProjectAbsent
}

// pipMissingDistribution returns the first requirement pip reported it could
// not find a distribution for, from pip's own lines (a build backend's relayed
// output cannot forge it). "".
func pipMissingDistribution(result *ExecResult) string {
	for _, line := range pipOwnLines(result) {
		if m := pipMissingDistRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// execSandboxed launches a wrapped verification command (see
// execWithPolicyLaunch for userNet) and reports a bubblewrap setup failure as
// an error.
func (p *Project) execSandboxed(ctx context.Context, wrappedCmd string, wrappedArgs []string, userNet *userNetLaunch) (*ExecResult, error) {
	result, err := p.execWithPolicyLaunch(ctx, wrappedCmd, wrappedArgs, isolatedExecPolicy(), userNet)
	if err == nil && isBwrapSetupFailure(result) {
		// bubblewrap itself failed before the command started; this is a
		// sandbox/environment problem, not a failing project command, so it
		// must not reach auto-fix or quality statistics as a test failure.
		return result, fmt.Errorf("verification sandbox failed to start: %s", firstLine(result.Stderr))
	}
	return result, err
}

// A failed dependency install is a registry outage (an environment problem,
// never evidence about the candidate) only when the install could not reach a
// registry it needed. What the install printed only raises that suspicion:
// the package manager's own diagnostics that a registry, package index or
// module proxy could not be reached (npm's error code for a failed request,
// pip's exhausted connection retries for an index page, the go command's
// failed module proxy or checksum database request, cargo's failed registry
// or index download). Candidate code can print such lines (npm lifecycle
// scripts, setup.py and build backends, build.rs), and project configuration
// (.npmrc, a requirements file's --index-url, .cargo/config.toml) can point
// the install at a host that does not answer, so they never decide the
// verdict. Output the project shapes does not even raise the suspicion where
// it can be told apart: compiler diagnostics quoting source lines, build
// backend and build script output, server responses the tool quotes, fetches
// of URLs the project names itself (a dependency declared by URL or git
// repository), and the project strings the go command prints as they are
// (go.mod tokens, import paths; see goProxyFailure).
//
// A step with outbound network (the user-mode network helper, or the host
// under the unsafe opt-in) had an outage only when makewand's own check of the
// ecosystem's default registry, from the same kind of network, gets no
// response (checkRegistryReachable). When the check reaches the registry, or
// cannot run at all (twice), the failure stays the command's.
//
// A step without outbound network (no user-mode network helper, or one that
// could not be attached) reaches no registry at all, so the only question is
// whether it needed one, and output project code can print cannot answer it:
// offlineRegistryNeed answers it from facts printing cannot change.
//
// When the suspicion is not confirmed, ExecResult.Note says why.

// registryOutageNote opens every ExecResult.Note.
const registryOutageNote = "the output suggests that a package registry could not be reached, but "

// dependencyRegistryError returns an error wrapping
// ErrDependencyNetworkUnavailable, with guidance, when the dependency install
// plan failed because a registry it needed is unreachable from where it ran.
// When the output only suggested that, the failure stays the command's and
// note says why the suggestion was not confirmed (ExecResult.Note).
func (p *Project) dependencyRegistryError(ctx context.Context, env verifyExecEnvironment, plan ExecPlan, result *ExecResult, network sandboxNetwork) (note string, err error) {
	evidence := p.registryFailureEvidence(plan, result)
	if evidence == "" {
		return "", nil
	}
	if len(evidence) > 240 {
		evidence = evidence[:240] + "..."
	}
	unavailable := func(guidance string) error {
		return fmt.Errorf("%w (%s): %s", ErrDependencyNetworkUnavailable, evidence, guidance)
	}
	const ownFailure = ", so this is reported as a failure of the install itself"
	if network.prefetched {
		// The loopback build/install stage of a split plan: its dependencies
		// were prefetched into the wheelhouse, so it reached no registry. Its
		// output cannot reclassify it (a build backend can print pip's lines),
		// and offlineRegistryNeed is stale (declared deps are prefetched), so
		// the failure stays the candidate's.
		return registryOutageNote + "the dependencies were prefetched and the build installed from them offline" + ownFailure, nil
	}
	if network.kind == sandboxNetworkNone {
		if p.offlineRegistryNeed(plan, evidence) == "" {
			return registryOutageNote + "the install ran without outbound network and makewand found nothing it had to download from a package registry" + ownFailure, nil
		}
		return "", unavailable(network.guidance())
	}
	check := checkRegistry(ctx, env, plan.Command)
	switch {
	case check.done && len(check.unreachable) > 0:
		return "", unavailable(network.unreachableGuidance(check.unreachable))
	case check.done:
		return registryOutageNote + "makewand's own connectivity check reached " + strings.Join(check.reached, ", ") + ownFailure +
			" (the project may name a registry, index or mirror that does not answer)", nil
	case ctx.Err() != nil:
		return "", nil
	}
	ran := "had outbound network through " + userNetHelperName
	if network.kind == sandboxNetworkHost {
		ran = "ran with the host network"
	}
	return registryOutageNote + "makewand could not confirm it: its own connectivity check could not run (" + check.failure +
		"), while the install itself " + ran + ownFailure, nil
}

// registryFailureEvidence returns the package manager's diagnostic suggesting
// that the failed dependency install plan could not reach its registry, or "".
func (p *Project) registryFailureEvidence(plan ExecPlan, result *ExecResult) string {
	if plan.Kind != "deps" || result == nil || result.ExitCode == 0 {
		return ""
	}
	switch plan.Command {
	case "npm":
		return p.npmRegistryFailure(result)
	case "pip", "pip3":
		return pipIndexFailure(result)
	case "go":
		return p.goProxyEvidence(result)
	case "cargo":
		return cargoRegistryFailure(result)
	}
	return ""
}

// outputLines returns the stderr and then the stdout lines of result.
func outputLines(result *ExecResult) []string {
	var lines []string
	for _, stream := range []string{result.Stderr, result.Stdout} {
		for _, line := range strings.Split(stream, "\n") {
			lines = append(lines, strings.TrimRight(line, " \t\r"))
		}
	}
	return lines
}

// npmNetworkErrno lists the errors of a request that never got a response
// (DNS, connect, reset, timeout).
const npmNetworkErrno = `ENOTFOUND|EAI_AGAIN|EAI_FAIL|ECONNREFUSED|ECONNRESET|ETIMEDOUT|ESOCKETTIMEDOUT|ERR_SOCKET_TIMEOUT|ENETUNREACH|EHOSTUNREACH|ECONNECTIONTIMEOUT|EIDLETIMEOUT|ERESPONSETIMEOUT|ETRANSFERTIMEOUT`

var (
	// npmNetworkCodeRe matches npm's error code line for such a request.
	npmNetworkCodeRe = regexp.MustCompile(`^npm (?:ERR!|error) code (?:` + npmNetworkErrno + `)$`)
	// npmFailedRequestRe captures the URL of npm's failed request, and
	// whether its reason is such an error.
	npmFailedRequestRe = regexp.MustCompile(`^npm (?:ERR!|error) .*\brequest to (\S+) failed(, reason: (?:getaddrinfo|connect|read|write) (?:` + npmNetworkErrno + `)\b)?`)
)

// npmRegistryFailure returns npm's diagnostic for a registry request that got
// no response, or "" when the install failed for another reason or the
// failed request was for a URL dependency that package.json declares.
func (p *Project) npmRegistryFailure(result *ExecResult) string {
	code, request, requestURL, unreachable := "", "", "", false
	for _, line := range outputLines(result) {
		if code == "" && npmNetworkCodeRe.MatchString(line) {
			code = line
		}
		if m := npmFailedRequestRe.FindStringSubmatch(line); m != nil && request == "" {
			request, requestURL, unreachable = line, m[1], m[2] != ""
		}
	}
	switch {
	case code == "" && !unreachable:
		return ""
	case requestURL != "" && p.npmDeclaresURLHost(urlHost(requestURL)):
		return ""
	case request != "":
		return request
	default:
		return code
	}
}

// npmDeclaresURLHost reports whether a dependency in package.json is fetched
// from host by URL (a tarball or git URL) rather than from the registry.
func (p *Project) npmDeclaresURLHost(host string) bool {
	if host == "" {
		return false
	}
	content, err := p.ReadFile("package.json")
	if err != nil {
		return false
	}
	var manifest map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &manifest) != nil {
		return false
	}
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var specs map[string]string
		if json.Unmarshal(manifest[field], &specs) != nil {
			continue
		}
		for _, spec := range specs {
			if urlHost(spec) == host {
				return true
			}
		}
	}
	return false
}

// urlHost returns the lower-case host of an absolute URL (git+ URLs
// included), or "".
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimPrefix(strings.TrimSpace(raw), "git+"))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

var (
	// pipRetriesExhaustedRe matches urllib3's last retry warning (no retries
	// left) for a connection-level failure, as pip prints it.
	pipRetriesExhaustedRe = regexp.MustCompile(`^WARNING: Retrying \(Retry\(total=0\b.*\)\) after connection broken by '(NewConnectionError|NameResolutionError|ConnectTimeoutError|ReadTimeoutError|ProtocolError|ProxyError)\(`)
	// pipNoDistributionRe matches pip's verdict for a requirement whose index
	// page offered no usable version.
	pipNoDistributionRe = regexp.MustCompile(`^ERROR: (?:Could not find a version that satisfies the requirement|No matching distribution found for) `)
	// pipMissingDistRe captures the requirement pip could not find a
	// distribution for (under --only-binary, a source-only distribution reads
	// as none available).
	pipMissingDistRe = regexp.MustCompile(`^ERROR: (?:Could not find a version that satisfies the requirement|No matching distribution found for) (\S+)`)
	// pipSubprocessHeaderRe matches the header of a subprocess failure box.
	pipSubprocessHeaderRe = regexp.MustCompile(`^× (.+) did not run successfully\.$`)
	// pipOwnSubprocessRe matches the subprocesses that are pip itself
	// (installing build requirements); other boxes relay build backend output.
	pipOwnSubprocessRe = regexp.MustCompile(`^(?:pip subprocess to install|installing) (?:build|backend) dependencies$`)
)

// pipIndexFailure returns pip's last connection retry warning when pip gave
// up on an index page and therefore found no distribution, or "". A direct
// URL requirement that cannot be fetched ends in a traceback instead and is
// not an index failure.
func pipIndexFailure(result *ExecResult) string {
	retry, noDistribution := "", false
	for _, line := range pipOwnLines(result) {
		if retry == "" && pipRetriesExhaustedRe.MatchString(line) {
			retry = line
		}
		if pipNoDistributionRe.MatchString(line) {
			noDistribution = true
		}
	}
	if !noDistribution {
		return ""
	}
	return retry
}

// pipOwnLines returns pip's own log lines, trimmed. Build backend output that
// pip relays in a "did not run successfully" box (the project's setup.py, for
// example) is left out; the box of pip's own build-requirements install is
// kept.
func pipOwnLines(result *ExecResult) []string {
	var own []string
	header, inBox, keep := "", false, false
	for _, raw := range outputLines(result) {
		line := strings.TrimSpace(raw)
		switch {
		case inBox && line == "[end of output]":
			inBox, header = false, ""
		case inBox:
			if keep {
				own = append(own, line)
			}
		case strings.HasPrefix(line, "╰─> [") && strings.HasSuffix(line, " of output]"):
			inBox, keep = true, pipOwnSubprocessRe.MatchString(header)
		default:
			if strings.HasPrefix(line, "× ") {
				header = ""
				if m := pipSubprocessHeaderRe.FindStringSubmatch(line); m != nil {
					header = m[1]
				}
			}
			own = append(own, line)
		}
	}
	return own
}

var (
	// goFetchFailureRe matches the go command's report of an HTTP request
	// that got no response.
	goFetchFailureRe = regexp.MustCompile(`\bGet "(https?://[^"]+)": (?:dial (?:tcp|udp)|read (?:tcp|udp)|write (?:tcp|udp)|proxyconnect tcp|net/http: (?:TLS handshake timeout|timeout awaiting response headers|request canceled while waiting for connection)|context deadline exceeded \(Client\.Timeout)`)
	// goModParseFailureRe matches the line the go command opens its report
	// with when it cannot parse go.mod or go.work.
	goModParseFailureRe = regexp.MustCompile(`^go: errors parsing (?:.*/)?go\.(?:mod|work):$`)
)

// goProxyFailure returns the go command's line reporting a module proxy or
// checksum database request that got no response, or "". The go command
// prints some project strings as they are - go.mod tokens (in its report of a
// go.mod it cannot parse, a replacement directory, a quoted module path) and
// import paths - so such a line can be shaped by the project:
//   - a go command that could not parse go.mod or go.work made no request;
//   - a request whose report lies in one of projectStrings (strings of the
//     project holding `Get "`, see goProjectStrings) is not counted.
func goProxyFailure(result *ExecResult, projectStrings ...string) string {
	lines := outputLines(result)
	for _, line := range lines {
		if goModParseFailureRe.MatchString(strings.TrimSpace(line)) {
			return ""
		}
	}
	for _, line := range lines {
		own := line
		for _, s := range projectStrings {
			own = strings.ReplaceAll(own, s, "\x00")
		}
		for _, m := range goFetchFailureRe.FindAllStringSubmatch(own, -1) {
			if goModuleServiceURL(m[1]) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// goProxyEvidence is goProxyFailure for the project's own go command run:
// a line only counts when it is not made of strings of the project that the go
// command printed as they are. When those strings cannot all be collected,
// no line counts.
func (p *Project) goProxyEvidence(result *ExecResult) string {
	if goProxyFailure(result) == "" {
		return ""
	}
	strs, ok := p.goProjectStrings()
	if !ok {
		return ""
	}
	return goProxyFailure(result, strs...)
}

// goModuleServiceURL reports whether rawURL is a GOPROXY protocol or checksum
// database request. Module paths the go command resolves itself (?go-get=1
// discovery, including such requests quoted from a proxy's response) are
// not.
func goModuleServiceURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Query().Has("go-get") {
		return false
	}
	return strings.Contains(u.Path, "/@v/") || strings.HasSuffix(u.Path, "/@latest") ||
		strings.Contains(u.Path, "/sumdb/") || strings.HasPrefix(u.Path, "/lookup/") || strings.HasPrefix(u.Path, "/tile/")
}

var (
	// cargoBuildStartedRe matches cargo's status line for compiling a crate;
	// after it, output can include rustc and build script diagnostics.
	cargoBuildStartedRe = regexp.MustCompile(`^\s+(?:Compiling|Checking) \S`)
	// cargoRegistryFetchRe matches cargo's errors for registry index and
	// crate downloads (git dependencies fail with other messages).
	cargoRegistryFetchRe = regexp.MustCompile("download of \\S+ failed|failed to download from `|failed to fetch `|failed to query replaced source registry `|failed to update registry `|Unable to update registry `")
	// cargoNetworkCauseRe matches a curl error that got no response, or a
	// libgit2 network error (git index).
	cargoNetworkCauseRe = regexp.MustCompile(`^\[(?:5|6|7|16|18|28|35|52|55|56)\] |class=Net \(12\)`)
)

// cargoRegistryFailure returns cargo's network error when cargo failed to
// download a registry index or crate before compiling anything, or "".
func cargoRegistryFailure(result *ExecResult) string {
	failed, registry := false, false
	cause := ""
	for _, raw := range outputLines(result) {
		if cargoBuildStartedRe.MatchString(raw) {
			return ""
		}
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "error: ") {
			failed = true
		}
		if cargoRegistryFetchRe.MatchString(line) {
			registry = true
		}
		if cause == "" && (cargoNetworkCauseRe.MatchString(line) || strings.HasPrefix(raw, "warning: spurious network error (")) {
			cause = line
		}
	}
	if !failed || !registry {
		return ""
	}
	return cause
}

// offlineRegistryNeed returns what shows that a failed dependency install
// without outbound network needed a package registry, or "" when nothing
// does. Such an install reaches no registry at all, so the only question is
// whether it needed one, and output that project code can print cannot answer
// it. Where project code can run during the install (npm lifecycle scripts or
// programs the project's .npmrc names, the setup.py or build backend of a
// local Python project, cargo build scripts or a rustc wrapper its
// configuration names), the project's files answer it: a registry dependency
// the project declares that is not installed (npmRegistryNeed,
// pipRegistryNeed, cargoRegistryNeed). `go mod tidy` and `go mod download`
// run no project code but print project strings as they are (go.mod tokens,
// import paths), so go.mod, go.sum, the project's imports and the module
// caches answer it (goRegistryNeed). Where no project code runs and nothing
// the project wrote is printed - pip installing only named requirements - the
// package manager's own report of the failed request (evidence) answers it.
//
// What remains: a candidate that declares a registry dependency that is not
// installed gets the environment verdict even when its own code fails first
// (without a registry it cannot be verified either way); an installed
// distribution or package counts whatever its version; what earlier steps
// left in a persistent sandbox HOME (a verification clone starts with an
// empty one) is not considered: caches pip, npm and cargo could install from
// offline, tool configuration, and code such as a user-site .pth file that
// runs in every Python process; and a need the files do not show (build
// requirements pip installs for a setup.py project without --use-pep517, a
// transitive npm dependency) leaves a failure that was the environment's to
// the command.
func (p *Project) offlineRegistryNeed(plan ExecPlan, evidence string) string {
	switch plan.Command {
	case "npm":
		return p.npmRegistryNeed()
	case "pip", "pip3":
		return p.pipRegistryNeed(plan, evidence)
	case "go":
		return p.goRegistryNeed(plan)
	case "cargo":
		return p.cargoRegistryNeed(plan)
	}
	return ""
}

// goModFile is go.mod as the go command parses it (`go mod edit -json`).
type goModFile struct {
	Module  struct{ Path string }
	Go      string
	Require []goModVersion
	Exclude []goModVersion
	Replace []struct{ Old, New goModVersion }
	Tool    []struct{ Path string }
	Ignore  []struct{ Path string }
}

// goModVersion is a module path and version in goModFile; a replacement
// without a version is a local directory.
type goModVersion struct {
	Path, Version string
}

// goModParse parses go.mod content with the host go toolchain goBin (the one
// the sandbox runs). A test seam.
var goModParse = parseGoModWithGoCommand

// parseGoModWithGoCommand parses go.mod content exactly as the go command
// parses a main module's go.mod, with `go mod edit -json`. It never runs in
// the workspace: a copy of the file is parsed from the root directory with
// GOTOOLCHAIN=local, so the file can neither switch toolchains nor make the
// go command fetch anything (go mod edit resolves no version).
func parseGoModWithGoCommand(goBin string, data []byte) (*goModFile, error) {
	dir, err := os.MkdirTemp("", "makewand-gomod-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	//nolint:gosec // G204: goBin is the host toolchain resolved from the sandbox PATH; fixed argv, no shell.
	cmd := exec.CommandContext(ctx, goBin, "mod", "edit", "-json", file)
	cmd.Dir = string(filepath.Separator)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go mod edit: %w", err)
	}
	var mod goModFile
	if err := json.Unmarshal(out, &mod); err != nil {
		return nil, fmt.Errorf("parse go mod edit output: %w", err)
	}
	return &mod, nil
}

// goRegistryNeed returns a module that the go plan - `go mod tidy`, or `go
// mod download` of the main module's requirements - has to fetch over the
// network, or "". It answers from facts the go command's output cannot
// change: go.mod as the go command parses it, go.sum, the imports of the main
// module's Go files and the module caches the sandboxed go command reads (its
// own in the sandbox HOME, and the host's, which the sandbox serves as a
// local GOPROXY entry). A module is needed when the go command has to load it
// and its .mod or .zip file is in neither cache, or when go.sum lacks its
// checksum and the go command asks the checksum database (goSumDB,
// GONOSUMDB, GOPRIVATE) for it. `go mod tidy` loads every requirement of a
// go.mod before go 1.17, and otherwise the modules that provide the main
// module's imports; an import no requirement provides is looked up in the
// module proxies. `go mod download` loads every requirement.
//
// It returns "" whenever the go command fails before fetching anything (a
// go.mod it cannot parse, an invalid main module path, a go.work in the
// project) and for what it cannot tell offline: module paths and versions the
// go command could not fetch at all, local replacements, excluded versions,
// `go mod download` of named modules. A need the files do not show (a
// requirement of a dependency, a package that the required module turns out
// not to hold) leaves a failure that was the environment's to the command.
func (p *Project) goRegistryNeed(plan ExecPlan) string {
	if len(plan.Args) < 2 || plan.Args[0] != "mod" || (plan.Args[1] != "tidy" && plan.Args[1] != "download") {
		return ""
	}
	tidy := plan.Args[1] == "tidy"
	if !tidy {
		for _, arg := range plan.Args[2:] {
			if !strings.HasPrefix(arg, "-") && arg != "all" {
				return ""
			}
		}
	}
	projectDir, err := p.execWorkingDir()
	if err != nil {
		return ""
	}
	// The go command looks for a go.work upwards from the project; workspace
	// mode is not modelled here.
	for dir := projectDir; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, "go.work")); err == nil {
			return ""
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	data, err := p.ReadFile("go.mod")
	if err != nil {
		return ""
	}
	goBin := lookPathIn("go", sandboxPathEnv())
	if goBin == "" {
		return ""
	}
	mod, err := goModParse(goBin, []byte(data))
	if err != nil || !goImportPathOK(mod.Module.Path) {
		return ""
	}
	hostEnv, err := sandboxGoEnvProbe(goBin)
	if err != nil {
		hostEnv = hostGoEnv{} // the sandbox then leaves the go command its defaults
	}
	sum, _ := p.ReadFile("go.sum")
	facts := newGoOfflineFacts(projectDir, hostEnv, sum)
	resolve := newGoModResolver(mod)
	if resolve == nil {
		return ""
	}
	if !tidy {
		for _, req := range mod.Require {
			if target, ok := resolve.module(req); ok {
				if need := facts.moduleNeed(target, "required in go.mod", true); need != "" {
					return need
				}
			}
		}
		return ""
	}
	if !goModPruned(mod.Go) {
		// Without module graph pruning the go command loads the go.mod file of
		// every requirement.
		for _, req := range mod.Require {
			if target, ok := resolve.module(req); ok {
				if need := facts.moduleNeed(target, "required in go.mod, whose module graph is not pruned", false); need != "" {
					return need
				}
			}
		}
	}
	var ignores []string
	for _, ignore := range mod.Ignore {
		ignores = append(ignores, ignore.Path)
	}
	imports := goModuleImports(projectDir, ignores)
	for _, tool := range mod.Tool {
		if _, seen := imports[tool.Path]; !seen {
			imports[tool.Path] = "go.mod (tool directive)"
		}
	}
	for _, path := range slices.Sorted(maps.Keys(imports)) {
		if !goImportNeedsModule(path) || path == mod.Module.Path || strings.HasPrefix(path, mod.Module.Path+"/") {
			continue
		}
		file := imports[path]
		providers, covered := resolve.providers(path)
		if !covered {
			if need := facts.lookupNeed(path, file); need != "" {
				return need
			}
			continue
		}
		for _, provider := range providers {
			if need := facts.moduleNeed(provider, "provides "+path+", imported by "+file, true); need != "" {
				return need
			}
		}
	}
	return ""
}

// goModPruned reports whether a go.mod `go` version prunes the module graph
// (go 1.17 or later; no directive means go 1.16).
func goModPruned(goVersion string) bool {
	v := "go" + strings.TrimSpace(goVersion)
	return version.IsValid(v) && version.Compare(v, "go1.17") >= 0
}

// goModResolver applies go.mod's replacements and exclusions to modules.
type goModResolver struct {
	mod      *goModFile
	excluded map[goModVersion]bool
}

// newGoModResolver returns the resolver of mod, or nil when mod requires a
// module twice (the go command rejects that before loading anything).
func newGoModResolver(mod *goModFile) *goModResolver {
	seen := map[string]bool{}
	for _, req := range mod.Require {
		if seen[req.Path] {
			return nil
		}
		seen[req.Path] = true
	}
	r := &goModResolver{mod: mod, excluded: map[goModVersion]bool{}}
	for _, ex := range mod.Exclude {
		r.excluded[ex] = true
	}
	return r
}

// module returns what the go command fetches for the module m: m itself or
// its replacement module, false for a local replacement, an excluded version
// (the go command picks another one) and a path or version it cannot fetch.
func (r *goModResolver) module(m goModVersion) (goModVersion, bool) {
	if r.excluded[m] {
		return goModVersion{}, false
	}
	target, replaced := m, false
	for _, rep := range r.mod.Replace {
		if rep.Old.Path != m.Path {
			continue
		}
		if rep.Old.Version == m.Version {
			target, replaced = rep.New, true
			break
		}
		if rep.Old.Version == "" && !replaced {
			target, replaced = rep.New, true
		}
	}
	if replaced && target.Version == "" {
		return goModVersion{}, false // a local directory
	}
	return target, goModulePathOK(target.Path) && goCanonicalVersionRe.MatchString(target.Version)
}

// providers returns the modules that may provide the package path: each
// requirement whose path is a prefix of it, resolved (module). covered
// reports whether a requirement or a replacement matches at all.
func (r *goModResolver) providers(path string) (providers []goModVersion, covered bool) {
	within := func(module string) bool { return path == module || strings.HasPrefix(path, module+"/") }
	for _, req := range r.mod.Require {
		if within(req.Path) {
			covered = true
			if target, ok := r.module(req); ok {
				providers = append(providers, target)
			}
		}
	}
	for _, rep := range r.mod.Replace {
		// A replaced module the go.mod does not require is resolved through
		// its replacement.
		covered = covered || within(rep.Old.Path)
	}
	return providers, covered
}

// goOfflineFacts is what the sandboxed go command can fetch without network.
type goOfflineFacts struct {
	// caches are the module cache download directories it reads: its own
	// (in the sandbox HOME) and the host's local GOPROXY entry.
	caches []string
	// sandboxCache is its own download directory.
	sandboxCache string
	// sums are go.sum's "path version" and "path version/go.mod" entries.
	sums map[string]bool
	// sumModules are the module paths go.sum has entries for.
	sumModules map[string]bool
	// sumdb is the checksum database it asks over the network, "" for none.
	sumdb string
	// noSumDB are the GONOSUMDB (GOPRIVATE) patterns.
	noSumDB string
	// proxyNetwork reports a network proxy or "direct" before any "off"
	// entry in GOPROXY; noProxy are the GONOPROXY (GOPRIVATE) patterns of
	// modules it fetches directly.
	proxyNetwork bool
	noProxy      string
}

func newGoOfflineFacts(projectDir string, hostEnv hostGoEnv, sum string) *goOfflineFacts {
	f := &goOfflineFacts{
		sandboxCache: filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath), "go", "pkg", "mod", "cache", "download"),
		sums:         map[string]bool{},
		sumModules:   map[string]bool{},
		noSumDB:      goNoSumDBPatterns(hostEnv),
	}
	f.caches = []string{f.sandboxCache}
	_, hostCache := sandboxGoProxy(hostEnv)
	if hostCache != "" {
		f.caches = append(f.caches, hostCache)
	}
	for _, line := range strings.Split(sum, "\n") {
		if fields := strings.Fields(line); len(fields) == 3 {
			f.sums[fields[0]+" "+fields[1]] = true
			f.sumModules[fields[0]] = true
		}
	}
	if name, explicit, ok := goSumDB(hostEnv.GOSUMDB); ok {
		f.sumdb = name
		// A local GOPROXY entry that serves the database answers its lookups.
		if explicit == "" && hostCache != "" && regularFileExists(filepath.Join(hostCache, "sumdb", name, "supported")) {
			f.sumdb = ""
		}
	}
	for _, proxy := range sandboxGoProxies(hostEnv) {
		if proxy.url == "off" {
			break
		}
		if proxy.url == "direct" || isNetworkGoProxy(proxy.url) {
			f.proxyNetwork = true
			break
		}
	}
	for _, value := range []string{hostEnv.GONOPROXY, hostEnv.GOPRIVATE} {
		if value = strings.TrimSpace(value); value != "" && !strings.Contains(value, "@") {
			f.noProxy = value
			break
		}
	}
	return f
}

// fetchesOverNetwork reports whether the go command fetches a module missing
// from the caches over the network.
func (f *goOfflineFacts) fetchesOverNetwork(path string) bool {
	return f.proxyNetwork || (f.noProxy != "" && goMatchPrefixPatterns(f.noProxy, path))
}

// cached reports whether a cache holds the module file with extension ext.
func (f *goOfflineFacts) cached(m goModVersion, ext string) bool {
	escPath, okPath := goModuleEscape(m.Path)
	escVersion, okVersion := goModuleEscape(m.Version)
	if !okPath || !okVersion {
		return false
	}
	for _, cache := range f.caches {
		if regularFileExists(filepath.Join(cache, filepath.FromSlash(escPath), "@v", escVersion+ext)) {
			return true
		}
	}
	return false
}

// asksSumDB reports whether the go command asks the checksum database over
// the network about a module path (version "" for any version).
func (f *goOfflineFacts) asksSumDB(m goModVersion) bool {
	if f.sumdb == "" || goMatchPrefixPatterns(f.noSumDB, m.Path) {
		return false
	}
	if m.Version != "" {
		escPath, okPath := goModuleEscape(m.Path)
		escVersion, okVersion := goModuleEscape(m.Version)
		if okPath && okVersion && regularFileExists(filepath.Join(f.sandboxCache, "sumdb", f.sumdb, "lookup", filepath.FromSlash(escPath)+"@"+escVersion)) {
			return false // an earlier lookup the go command kept
		}
	}
	return true
}

// moduleNeed returns why the go command fetches module m over the network to
// load it (its .mod file, and its .zip when zip is set), or "".
func (f *goOfflineFacts) moduleNeed(m goModVersion, why string, zip bool) string {
	if !f.cached(m, ".mod") || (zip && !f.cached(m, ".zip")) {
		if !f.fetchesOverNetwork(m.Path) {
			return ""
		}
		return m.Path + " " + m.Version + " (" + why + "; not in the sandbox's or the host's module cache)"
	}
	if (!f.sums[m.Path+" "+m.Version+"/go.mod"] || (zip && !f.sums[m.Path+" "+m.Version])) && f.asksSumDB(m) {
		return m.Path + " " + m.Version + " (" + why + "; go.sum has no checksum for it, so the go command asks the checksum database " + f.sumdb + ")"
	}
	return ""
}

// lookupNeed returns why the go command, resolving an import that no
// requirement provides, has to use the network, or "". It looks up each path
// prefix that is a valid module path, the longest first: when that lookup
// fails over the network the resolution fails, unless only local GOPROXY
// entries are used, where a module listed under a shorter prefix is found
// offline. A module found offline still needs its checksums: go.sum or the
// checksum database.
func (f *goOfflineFacts) lookupNeed(path, file string) string {
	why := path + " (imported by " + file + "; no requirement in go.mod provides it"
	for prefix := path; ; {
		if goModulePathOK(prefix) {
			if f.listed(prefix) {
				if f.sumModules[prefix] || !f.asksSumDB(goModVersion{Path: prefix}) {
					return ""
				}
				return why + ", and go.sum has no checksum for " + prefix + ", so the go command asks the checksum database " + f.sumdb + ")"
			}
			if f.fetchesOverNetwork(path) {
				return why + ", so go mod tidy looks it up in the module proxy)"
			}
		}
		i := strings.LastIndex(prefix, "/")
		if i < 0 {
			return ""
		}
		prefix = prefix[:i]
	}
}

// listed reports whether a cache lists versions of the module path.
func (f *goOfflineFacts) listed(path string) bool {
	escPath, ok := goModuleEscape(path)
	if !ok {
		return false
	}
	for _, cache := range f.caches {
		if info, err := os.Stat(filepath.Join(cache, filepath.FromSlash(escPath), "@v", "list")); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return true
		}
	}
	return false
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// goCanonicalVersionRe matches a canonical module version (pseudo-versions
// included).
var goCanonicalVersionRe = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+incompatible)?$`)

// goImportPathOK reports whether path is an import path the go command
// accepts (module.CheckImportPath, conservatively).
func goImportPathOK(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false
	}
	for _, elem := range strings.Split(path, "/") {
		if elem == "" || elem[0] == '.' || elem[len(elem)-1] == '.' {
			return false
		}
		for _, r := range elem {
			if !goImportPathRune(r) {
				return false
			}
		}
	}
	return true
}

// goImportPathRune reports whether r may appear in an import path.
func goImportPathRune(r rune) bool {
	return r == '-' || r == '.' || r == '_' || r == '~' || r == '+' ||
		'0' <= r && r <= '9' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z'
}

// goDomainRune reports whether r may appear in the first element of a module
// path (a lower-case domain name).
func goDomainRune(r rune) bool {
	return r == '-' || r == '.' || '0' <= r && r <= '9' || 'a' <= r && r <= 'z'
}

// goModulePathOK reports whether path is a module path the go command can
// fetch (module.CheckPath, conservatively): an import path without '+' whose
// first element is a lower-case domain name with a dot.
func goModulePathOK(path string) bool {
	if !goImportPathOK(path) || strings.Contains(path, "+") {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") || first[0] == '-' {
		return false
	}
	for _, r := range first {
		if !goDomainRune(r) {
			return false
		}
	}
	return true
}

// goImportNeedsModule reports whether the go command resolves an import path
// through a module: a valid import path whose first element is a module
// path's (a lower-case domain name with a dot), so not the cgo
// pseudo-package, a standard library path or one no module can provide.
func goImportNeedsModule(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return goImportPathOK(path) && goModulePathOK(first)
}

// goModuleEscape escapes a module path or version for the module cache
// (module.EscapePath): an upper-case letter becomes '!' and its lower case.
func goModuleEscape(s string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '!' || r >= 0x80:
			return "", false
		case 'A' <= r && r <= 'Z':
			b.WriteByte('!')
			b.WriteRune(r + 'a' - 'A')
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), s != ""
}

// goMatchPrefixPatterns reports whether a path prefix of target matches one
// of the comma-separated glob patterns (module.MatchPrefixPatterns, as
// GOPRIVATE, GONOPROXY and GONOSUMDB are matched).
func goMatchPrefixPatterns(globs, target string) bool {
	for _, glob := range strings.Split(globs, ",") {
		glob = strings.TrimSuffix(strings.TrimSpace(glob), "/")
		if glob == "" {
			continue
		}
		n := strings.Count(glob, "/")
		prefix := target
		for i := 0; i < len(target); i++ {
			if target[i] == '/' {
				if n == 0 {
					prefix = target[:i]
					break
				}
				n--
			}
		}
		if n > 0 {
			continue
		}
		if matched, _ := pathpkg.Match(glob, prefix); matched {
			return true
		}
	}
	return false
}

// goImportScanLimit bounds the Go files goModuleImports and goProjectStrings
// read, and goImportScanFileBytes each file.
const (
	goImportScanLimit     = 20000
	goImportScanFileBytes = 4 << 20
)

// goModuleImports returns the packages the main module's Go files import,
// each with the first file (relative to root) importing it: the files `go
// mod tidy` loads, for any build constraint except "ignore" and with tests,
// outside the directories the go command skips (.foo, _foo, testdata,
// vendor, nested modules, go.mod's ignore directives). Files it cannot parse
// are left out.
func goModuleImports(root string, ignores []string) map[string]string {
	imports := map[string]string{}
	files := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" ||
				goIgnoredDir(rel, ignores) || regularFileExists(filepath.Join(path, "go.mod")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		if files++; files > goImportScanLimit {
			return filepath.SkipAll
		}
		for _, imported := range goFileImports(path) {
			if _, seen := imports[imported]; !seen {
				imports[imported] = rel
			}
		}
		return nil
	})
	return imports
}

// goIgnoredDir reports whether a go.mod ignore directive covers the
// directory rel (slash-separated, relative to the module root): "./x" the
// tree x at the root, "x" any tree named x (search.IgnorePatterns).
func goIgnoredDir(rel string, ignores []string) bool {
	dir := "/" + rel + "/"
	for _, ignore := range ignores {
		pattern, relative := strings.CutPrefix(ignore, "./")
		pattern = "/" + strings.Trim(filepath.ToSlash(pattern), "/") + "/"
		if pattern == "//" {
			continue
		}
		if (relative && strings.HasPrefix(dir, pattern)) || (!relative && strings.Contains(dir, pattern)) {
			return true
		}
	}
	return false
}

// goFileImports returns the import paths of the Go file path, or none when
// it cannot be parsed or its build constraint holds for no configuration
// `go mod tidy` considers (any tag but "ignore").
func goFileImports(path string) []string {
	info, err := os.Stat(path)
	if err != nil || info.Size() > goImportScanFileBytes {
		return nil
	}
	src, err := os.ReadFile(path) //nolint:gosec // G304: a Go file of the project being verified
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ImportsOnly|parser.ParseComments)
	if err != nil || !goFileBuilds(file) {
		return nil
	}
	var imports []string
	for _, spec := range file.Imports {
		if value, err := strconv.Unquote(spec.Path.Value); err == nil {
			imports = append(imports, value)
		}
	}
	return imports
}

// goFileBuilds evaluates the file's build constraint the way `go mod tidy`
// does (cmd/go/internal/imports with tags {"*"}): every tag but "ignore"
// holds and does not hold at once.
func goFileBuilds(file *ast.File) bool {
	var plusBuild []constraint.Expr
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, c := range group.List {
			switch {
			case constraint.IsGoBuild(c.Text):
				expr, err := constraint.Parse(c.Text)
				return err == nil && goTidyTagsMatch(expr, true)
			case constraint.IsPlusBuild(c.Text):
				if expr, err := constraint.Parse(c.Text); err == nil {
					plusBuild = append(plusBuild, expr)
				}
			}
		}
	}
	for _, expr := range plusBuild {
		if !goTidyTagsMatch(expr, true) {
			return false
		}
	}
	return true
}

// goTidyTagsMatch is cmd/go/internal/imports' eval for the tags {"*"}.
func goTidyTagsMatch(x constraint.Expr, prefer bool) bool {
	switch x := x.(type) {
	case *constraint.TagExpr:
		if x.Tag == "ignore" {
			return false
		}
		return prefer
	case *constraint.NotExpr:
		return !goTidyTagsMatch(x.X, !prefer)
	case *constraint.AndExpr:
		return goTidyTagsMatch(x.X, prefer) && goTidyTagsMatch(x.Y, prefer)
	case *constraint.OrExpr:
		return goTidyTagsMatch(x.X, prefer) || goTidyTagsMatch(x.Y, prefer)
	}
	return false
}

// goProjectStringsLimit bounds the strings goProjectStrings returns.
const goProjectStringsLimit = 1024

// isSandboxHomeDir reports whether the directory path below root is the
// sandbox HOME makewand creates in the project (sandboxHomeRelPath).
func isSandboxHomeDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && strings.EqualFold(filepath.ToSlash(rel), sandboxHomeRelPath)
}

// goProjectStrings returns the strings of the project that the go command may
// print as they are and that hold `Get "` - what a forged report of a failed
// request needs: every token of each go.mod and go.work (string values
// unquoted, raw tokens as written), and every string literal of the Go files.
// It reads every such file in the project, since a replacement directory or
// an imported package can be anywhere in it - in a directory named .makewand
// too, which generated files may write. Skipped are only .git and the
// sandbox HOME makewand creates (sandboxHomeRelPath: the tool caches of the
// verification steps, the module cache among them). Direct generated-file
// writes there are blocked by isProtectedWritePath; test and auto-fix processes
// still run with that HOME and may write its caches. false means not every file
// could be read.
func (p *Project) goProjectStrings() ([]string, bool) {
	root, err := p.execWorkingDir()
	if err != nil {
		return nil, false
	}
	var strs []string
	seen := map[string]bool{}
	add := func(s string) {
		if strings.Contains(s, `Get "`) && !seen[s] {
			seen[s] = true
			strs = append(strs, s)
		}
	}
	files, complete := 0, true
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			complete = false
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == ".git" || isSandboxHomeDir(root, path)) {
				return filepath.SkipDir
			}
			return nil
		}
		isGo := strings.HasSuffix(name, ".go")
		if !isGo && name != "go.mod" && name != "go.work" {
			return nil
		}
		// The go command reads a symlinked file through its link.
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if files++; files > goImportScanLimit {
			complete = false
			return filepath.SkipAll
		}
		if info.Size() > goImportScanFileBytes {
			complete = false
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: a file of the project being verified
		if err != nil {
			complete = false
			return nil
		}
		if isGo {
			goStringLiterals(src, add)
		} else {
			goModTokens(string(src), add)
		}
		return nil
	})
	// Longer strings first, so a string inside another one does not break
	// the longer one apart before it is removed.
	slices.SortStableFunc(strs, func(a, b string) int { return len(b) - len(a) })
	return strs, complete && len(strs) <= goProjectStringsLimit
}

// goStringLiterals calls add with every string literal of the Go source src,
// as written and unquoted.
func goStringLiterals(src []byte, add func(string)) {
	var s scanner.Scanner
	fset := token.NewFileSet()
	s.Init(fset.AddFile("", fset.Base(), len(src)), src, func(token.Position, string) {}, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return
		}
		if tok == token.STRING {
			add(lit)
			if value, err := strconv.Unquote(lit); err == nil {
				add(value)
			}
		}
	}
}

// goModTokens calls add with every token of a go.mod or go.work file as
// written (a word, or a quoted string with its quotes) and with each quoted
// string unquoted (cmd/vendor/golang.org/x/mod/modfile's lexer: comments
// start with //, strings do not span lines).
func goModTokens(text string, add func(string)) {
	for _, line := range strings.Split(text, "\n") {
		for i := 0; i < len(line); {
			c := line[i]
			switch {
			case c == ' ' || c == '\t' || c == '\r' || c == '(' || c == ')' || c == '[' || c == ']' || c == '{' || c == '}' || c == ',':
				i++
			case strings.HasPrefix(line[i:], "//"):
				i = len(line)
			case c == '"' || c == '`':
				j := i + 1
				for j < len(line) && line[j] != c {
					if c == '"' && line[j] == '\\' {
						j++
					}
					j++
				}
				tok := line[i:min(j+1, len(line))]
				add(tok)
				if c == '`' {
					add(strings.Trim(tok, "`"))
				} else if value, err := strconv.Unquote(tok); err == nil {
					add(value)
				}
				i = j + 1
			default:
				j := i
				for j < len(line) && !strings.ContainsRune(" \t\r()[]{},", rune(line[j])) && !strings.HasPrefix(line[j:], "//") {
					j++
				}
				add(line[i:j])
				i = j
			}
		}
		add(line) // a whole line, as some reports print it
	}
}

// npmPackageNameRe matches a valid npm package name, scoped or not.
var npmPackageNameRe = regexp.MustCompile(`^(?:@[A-Za-z0-9][A-Za-z0-9._~-]*/)?[A-Za-z0-9][A-Za-z0-9._~-]*$`)

// networkFetchUnavailable returns, for a dependency plan whose networked fetch
// would otherwise run project code (or fetch a dependency it cannot get from a
// registry), why that cannot be done under fetch-only networking, or "". Only
// npm is affected: pip and cargo are split so the networked stage builds
// nothing, and go fetches no code. It applies only when outbound network is
// actually available: without the user-mode network helper the fetch runs
// loopback, so a prepare script cannot exfiltrate and the offline classifier
// (offlineRegistryNeed) decides instead, as it did before this change.
func (p *Project) networkFetchUnavailable(plan ExecPlan) string {
	switch plan.Command {
	case "npm", "pnpm", "yarn":
		if _, err := userNetLookPath(userNetHelperName); err != nil {
			return "" // no outbound network: the fetch runs loopback, nothing to refuse
		}
		if dep := p.npmNonRegistryDependency(); dep != "" {
			return "the project depends on " + dep + ", which npm cannot fetch from a registry without running project code (a local-directory or git dependency runs its prepare script even under --ignore-scripts); makewand only lets a dependency download use the network, so install the project's dependencies outside makewand"
		}
	}
	return ""
}

// npmNonRegistryDependency returns a dependency npm would fetch by running
// project code - a local path/directory (file:/link:/./) or a git repository
// (git+/git:/github:/gitlab:/bitbucket:/user'/repo shorthand), whose prepare
// script runs even under --ignore-scripts - or a workspace link outside a real
// workspaces setup (unfetchable), as "name (spec)", from package.json or the
// lockfile in use, or "". A plain http(s) tarball is not flagged: npm extracts
// it without running a prepare script (pacote's remote fetcher runs none), so
// it is a download, not code execution.
func (p *Project) npmNonRegistryDependency() string {
	if content, err := p.ReadFile("package.json"); err == nil {
		var manifest map[string]json.RawMessage
		if json.Unmarshal([]byte(content), &manifest) == nil {
			hasWorkspaces := len(manifest["workspaces"]) > 0 || p.npmHasWorkspaceConfig()
			for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
				var specs map[string]string
				if json.Unmarshal(manifest[field], &specs) != nil {
					continue
				}
				for _, name := range slices.Sorted(maps.Keys(specs)) {
					if npmSpecRunsCodeOnFetch(specs[name], hasWorkspaces) {
						return name + " (" + field + ": " + specs[name] + ")"
					}
				}
			}
		}
	}
	return p.npmLockfileNonRegistry()
}

// npmHasWorkspaceConfig reports whether a workspaces setup is declared outside
// package.json (pnpm-workspace.yaml), so a workspace: dependency links in place
// and fetches nothing.
func (p *Project) npmHasWorkspaceConfig() bool {
	_, err := p.ReadFile("pnpm-workspace.yaml")
	return err == nil
}

// npmSpecRunsCodeOnFetch reports whether npm fetching a package.json dependency
// spec runs project code (a local directory or git repository, whose prepare
// script runs even under --ignore-scripts), or cannot fetch it from a registry
// at all (a workspace: link without a workspaces setup). A version, range,
// tag, npm: alias, or plain http(s) tarball returns false (a registry resolve
// or a scriptless download).
func npmSpecRunsCodeOnFetch(spec string, hasWorkspaces bool) bool {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "", strings.HasPrefix(spec, "npm:"):
		return false
	case strings.HasPrefix(spec, "workspace:"):
		return !hasWorkspaces
	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		return false // a tarball URL: extracted, no prepare script runs
	case strings.HasPrefix(spec, "file:"), strings.HasPrefix(spec, "link:"):
		return true
	case strings.HasPrefix(spec, "git+"), strings.HasPrefix(spec, "git:"), strings.HasPrefix(spec, "git@"):
		return true
	case strings.HasPrefix(spec, "github:"), strings.HasPrefix(spec, "gitlab:"), strings.HasPrefix(spec, "bitbucket:"), strings.HasPrefix(spec, "gist:"):
		return true
	case strings.HasPrefix(spec, "."), strings.HasPrefix(spec, "~"), strings.HasPrefix(spec, "/"):
		return true // a local path
	}
	// A "user/repo" GitHub shorthand (optionally with a #committish, which may
	// itself be "semver:..."): a slash, and no scheme before the committish.
	base, _, _ := strings.Cut(spec, "#")
	return strings.Contains(base, "/") && !strings.Contains(base, ":")
}

// npmLockfileNonRegistry scans package-lock.json / npm-shrinkwrap.json for a
// package resolved from a non-registry source (a git repository, a local
// directory link, or a file: path), transitive ones included, and returns it
// as "name (spec)" or "". It reads the lockfile structurally, so a false
// "resolved" string in a dependency's own field cannot forge a registry source.
func (p *Project) npmLockfileNonRegistry() string {
	for _, name := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		content, err := p.ReadFile(name)
		if err != nil {
			continue
		}
		var lock struct {
			Packages map[string]struct {
				Resolved string `json:"resolved"`
				Link     bool   `json:"link"`
			} `json:"packages"`
			Dependencies map[string]struct {
				Version  string `json:"version"`
				Resolved string `json:"resolved"`
			} `json:"dependencies"`
		}
		if json.Unmarshal([]byte(content), &lock) != nil {
			continue
		}
		for path, pkg := range lock.Packages { // lockfile v2/v3
			if path == "" {
				continue // the root project itself
			}
			if pkg.Link || npmNonRegistrySource(pkg.Resolved) {
				return npmLockPackageName(path) + " (" + name + ")"
			}
		}
		for dep, pkg := range lock.Dependencies { // lockfile v1
			if npmNonRegistrySource(pkg.Resolved) || npmNonRegistrySource(pkg.Version) {
				return dep + " (" + name + ")"
			}
		}
	}
	return ""
}

// npmNonRegistrySource reports whether a lockfile resolved/version value names
// a git repository or a file: path (npm runs project code preparing those).
func npmNonRegistrySource(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "git+") || strings.HasPrefix(value, "git:") ||
		strings.HasPrefix(value, "file:") || strings.HasPrefix(value, "ssh:")
}

// npmLockPackageName returns the package name from a lockfile v2/v3 package
// path such as "node_modules/@scope/pkg" or "node_modules/a/node_modules/b".
func npmLockPackageName(path string) string {
	if i := strings.LastIndex(path, "node_modules/"); i >= 0 {
		return path[i+len("node_modules/"):]
	}
	return path
}

// npmRegistryNeed returns a registry dependency that package.json declares and
// node_modules does not hold, or "". devDependencies count unless an .npmrc
// may leave them out; optional and peer dependencies never count (a failed
// optional install is not fatal, and peers can be left out by configuration).
func (p *Project) npmRegistryNeed() string {
	content, err := p.ReadFile("package.json")
	if err != nil {
		return ""
	}
	var manifest map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &manifest) != nil {
		return ""
	}
	fields := []string{"dependencies", "devDependencies"}
	if p.npmMayOmitDevDependencies() {
		fields = fields[:1]
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		return ""
	}
	defer root.Close()
	for _, field := range fields {
		var specs map[string]string
		if json.Unmarshal(manifest[field], &specs) != nil {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(specs)) {
			if !npmPackageNameRe.MatchString(name) || !npmRegistrySpec(specs[name]) {
				continue
			}
			if _, err := root.Stat(filepath.Join("node_modules", filepath.FromSlash(name), "package.json")); err == nil {
				continue
			}
			return name + " (" + field + " in package.json)"
		}
	}
	return ""
}

// npmRegistrySpec reports whether a package.json dependency spec resolves from
// the registry: a version, range or tag, or an npm: alias, rather than a
// path, URL, git repository or workspace link.
func npmRegistrySpec(spec string) bool {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "npm:") {
		return true
	}
	return !strings.ContainsAny(spec, ":/\\") && !strings.HasPrefix(spec, ".")
}

// npmMayOmitDevDependencies reports whether an .npmrc of the project or of the
// sandbox HOME sets configuration that can leave devDependencies out.
func (p *Project) npmMayOmitDevDependencies() bool {
	for _, rc := range []string{".npmrc", filepath.Join(filepath.FromSlash(sandboxHomeRelPath), ".npmrc")} {
		content, err := p.ReadFile(rc)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(content, "\n") {
			key, _, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "omit", "include", "production", "only", "also", "dev":
				return true
			}
		}
	}
	return false
}

// pipInstall is what a pip install plan installs, as far as its arguments and
// requirement files tell.
type pipInstall struct {
	// requirements are the named requirements, resolved from the index.
	requirements []pipRequirement
	// downloadReqs is the text of every named index requirement (command line
	// and readable requirement files), markers kept, for the networked
	// `pip download`. A buildable target is never recorded here.
	downloadReqs []string
	// indexArgs are the install's index options, sanitised to http(s) URLs (and
	// bare trusted hosts), for the networked `pip download` so it resolves from
	// the same indexes the install would. A non-http(s) index is dropped here
	// and sets otherSources instead.
	indexArgs []string
	// constraintPins are the named version pins of the install's constraint
	// files (-c), for a sanitised constraints file the fetch resolves against,
	// so the fetch and the offline install pick the same versions. A buildable
	// constraint target is never recorded.
	constraintPins []string
	// customIndex is set when the install points pip at an index other than the
	// default PyPI (a command-line or requirement-file --index-url/--find-links,
	// or pip configuration): a nonexistent-project fact check then cannot run.
	customIndex bool
	// projects are the local project directories it builds (project-relative).
	projects []string
	// localCode is set when it builds a local project or source archive, or
	// reads requirements makewand cannot see: project code may run then.
	localCode bool
	// otherSources is set when distributions may come from elsewhere than a
	// package index on the network (--no-index, --find-links, a local index).
	otherSources bool
	noIsolation  bool // --no-build-isolation
	pep517       bool // --use-pep517
	noPEP517     bool // --no-use-pep517
	// read holds the requirement files read so far.
	read map[string]bool
}

// pipRequirement is a named requirement: its normalized project name, the
// requirement as written, and where it is declared.
type pipRequirement struct {
	name, text, source string
}

// pipOptionsWithValue are the pip install options that take a value.
var pipOptionsWithValue = map[string]bool{
	"-r": true, "--requirement": true, "-c": true, "--constraint": true, "-e": true, "--editable": true,
	"-i": true, "--index-url": true, "--extra-index-url": true, "-f": true, "--find-links": true,
	"-t": true, "--target": true, "--prefix": true, "--root": true, "--src": true, "--platform": true,
	"--python-version": true, "--implementation": true, "--abi": true, "--upgrade-strategy": true,
	"-C": true, "--config-settings": true, "--global-option": true, "--report": true, "--log": true,
	"--proxy": true, "--retries": true, "--timeout": true, "--exists-action": true, "--trusted-host": true,
	"--cert": true, "--client-cert": true, "--cache-dir": true, "--progress-bar": true, "--only-binary": true,
	"--no-binary": true, "--python": true, "--use-feature": true, "--use-deprecated": true,
	"--keyring-provider": true, "--group": true, "--hash": true,
}

var (
	// pipCommentRe matches a requirements file comment.
	pipCommentRe = regexp.MustCompile(`(?:^|\s)#.*$`)
	// pipTrailingOptionsRe matches the start of per-requirement options.
	pipTrailingOptionsRe = regexp.MustCompile(`\s--?[A-Za-z]`)
	// pipVCSRe matches a version control requirement.
	pipVCSRe = regexp.MustCompile(`^(?:git|hg|svn|bzr)\+`)
	// pipArchiveRe matches a local distribution archive.
	pipArchiveRe = regexp.MustCompile(`(?i)\.(?:whl|zip|tar|tgz|tar\.gz|tar\.bz2|tar\.xz)$`)
	// pipRequirementRe matches a requirement (PEP 508): the project name, then
	// extras, a version specifier, a direct reference or markers.
	pipRequirementRe = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)\s*(.*)$`)
	// pipNamedReqRe matches a requirement that resolves purely from a package
	// index - a PEP 503 name, optional extras, optional version specifiers -
	// and nothing else (no URL scheme, VCS prefix, file:, path or direct
	// reference). The environment marker is split off before matching.
	pipNamedReqRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?` + // name
		`(?:\s*\[\s*[A-Za-z0-9](?:[A-Za-z0-9._,\s-]*[A-Za-z0-9])?\s*\])?` + // extras
		`(?:\s*(?:===|==|!=|~=|<=|>=|<|>)\s*[A-Za-z0-9][A-Za-z0-9.*+!_-]*\s*,?)*` + // version specifiers
		`\s*$`)
	// pipDirectReferenceRe matches a requirement with a direct reference
	// (name @ url).
	pipDirectReferenceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\s*(?:\[[^\]]*\])?\s*@`)
	// pipNameSeparatorsRe matches what Python project name normalization
	// folds into a single dash (PEP 503).
	pipNameSeparatorsRe = regexp.MustCompile(`[-_.]+`)
)

// pipRegistryNeed returns, for a pip install that builds a local project
// (whose setup.py or build backend is project code), a requirement it had to
// download from a package index: a build requirement of an isolated build, or
// a requirement no installed distribution provides. For a pip install of
// named requirements only, pip's own report (evidence) answers. It returns ""
// when distributions may come from elsewhere: --no-index, --find-links, a
// local index, or pip configuration that sets one of those or turns build
// isolation off.
func (p *Project) pipRegistryNeed(plan ExecPlan, evidence string) string {
	install, ok := p.parsePipInstall(plan.Args)
	switch {
	case !ok:
		return ""
	case !install.localCode:
		return evidence
	case install.otherSources || p.pipConfigMayAvoidIndex():
		return ""
	}
	for _, dir := range install.projects {
		if need := p.pipBuildRequirementNeed(install, dir); need != "" {
			return need
		}
	}
	requirements := install.requirements
	for _, dir := range install.projects {
		requirements = append(requirements, p.pipStaticDependencies(dir)...)
	}
	if len(requirements) == 0 {
		return ""
	}
	installed := p.pipInstalledDistributions()
	for _, requirement := range requirements {
		if !installed[requirement.name] {
			return requirement.text + " (" + requirement.source + ")"
		}
	}
	return ""
}

// parsePipInstall reads a `pip install` command line.
func (p *Project) parsePipInstall(args []string) (pipInstall, bool) {
	if len(args) == 0 || args[0] != "install" {
		return pipInstall{}, false
	}
	var install pipInstall
	p.addPipArguments(&install, args[1:], "the pip command line", ".", 0)
	return install, true
}

// addPipArguments reads pip install arguments from the command line or from a
// line of the requirements file source; dir resolves requirement file paths.
func (p *Project) addPipArguments(install *pipInstall, args []string, source, dir string, depth int) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			p.addPipTarget(install, arg, source)
			continue
		}
		option, value, inline := strings.Cut(arg, "=")
		if !inline && len(option) > 2 && !strings.HasPrefix(option, "--") {
			option, value, inline = option[:2], option[2:], true // -rfile, -e.
		}
		if !inline && pipOptionsWithValue[option] && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch option {
		case "-r", "--requirement":
			if !filepath.IsAbs(value) {
				value = filepath.Join(dir, value)
			}
			p.addPipRequirementsFile(install, value, depth+1)
		case "-c", "--constraint":
			if !filepath.IsAbs(value) {
				value = filepath.Join(dir, value)
			}
			p.addPipConstraintsFile(install, value, depth+1)
		case "-e", "--editable":
			if !strings.Contains(value, "://") && !pipVCSRe.MatchString(value) {
				p.addPipLocal(install, value)
			}
		case "--no-index":
			install.otherSources = true
			install.customIndex = true
		case "-f", "--find-links":
			install.otherSources = true
			install.customIndex = true
			if pipHTTPURL(value) {
				install.indexArgs = append(install.indexArgs, "--find-links", value)
			}
		case "-i", "--index-url", "--extra-index-url":
			install.customIndex = true
			if pipHTTPURL(value) {
				name := option
				if name == "-i" {
					name = "--index-url"
				}
				install.indexArgs = append(install.indexArgs, name, value)
			} else {
				install.otherSources = true
			}
		case "--trusted-host":
			if pipTrustedHost(value) {
				install.indexArgs = append(install.indexArgs, "--trusted-host", value)
			}
		case "--no-build-isolation":
			install.noIsolation = true
		case "--use-pep517":
			install.pep517 = true
		case "--no-use-pep517":
			install.noPEP517 = true
		}
	}
}

// addPipRequirementsFile reads a requirements file of the project, once. One
// that makewand cannot read (outside the project, or nested too deeply) may
// hold anything.
func (p *Project) addPipRequirementsFile(install *pipInstall, file string, depth int) {
	file = filepath.Clean(file)
	if install.read[file] {
		return
	}
	if install.read == nil {
		install.read = map[string]bool{}
	}
	install.read[file] = true
	content, err := p.ReadFile(file)
	if err != nil || depth > 8 {
		install.localCode = true
		return
	}
	for _, line := range pipRequirementLines(content) {
		if strings.HasPrefix(line, "-") {
			p.addPipArguments(install, strings.Fields(line), file, filepath.Dir(file), depth)
			continue
		}
		if loc := pipTrailingOptionsRe.FindStringIndex(line); loc != nil {
			line = strings.TrimSpace(line[:loc[0]])
		}
		p.addPipTarget(install, line, file)
	}
}

// addPipConstraintsFile reads a constraint file (-c) of the project and
// records its named version pins for the sanitised constraints file the
// networked fetch resolves against. A constraint file may name a buildable
// target (a path, URL or VCS reference) too; those are never recorded, so the
// fetch never builds one. A file makewand cannot read leaves no pins (the
// offline install still reads the real file loopback-only).
func (p *Project) addPipConstraintsFile(install *pipInstall, file string, depth int) {
	file = filepath.Clean(file)
	if install.read[file] {
		return
	}
	if install.read == nil {
		install.read = map[string]bool{}
	}
	install.read[file] = true
	content, err := p.ReadFile(file)
	if err != nil || depth > 8 {
		return
	}
	for _, line := range pipRequirementLines(content) {
		if strings.HasPrefix(line, "-") {
			continue // options in a constraint file are not fetch targets
		}
		if loc := pipTrailingOptionsRe.FindStringIndex(line); loc != nil {
			line = strings.TrimSpace(line[:loc[0]])
		}
		if named, ok := pipNamedRequirementText(line); ok {
			install.constraintPins = append(install.constraintPins, named)
		}
	}
}

// pipHTTPURL reports whether value is an http(s) URL (the only index source a
// networked fetch is handed).
func pipHTTPURL(value string) bool {
	return strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://")
}

// pipTrustedHost reports whether value is a bare host[:port] (what
// --trusted-host takes), not a URL or path.
func pipTrustedHost(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.Contains(value, "://") && !strings.ContainsAny(value, "/\\@ \t")
}

// pipRequirementLines returns the logical lines of a requirements file:
// continuation lines joined, comments and blank lines dropped.
func pipRequirementLines(content string) []string {
	var lines []string
	var current strings.Builder
	for _, raw := range strings.Split(content, "\n") {
		raw = strings.TrimRight(raw, "\r")
		if continued, ok := strings.CutSuffix(raw, "\\"); ok {
			current.WriteString(continued)
			continue
		}
		current.WriteString(raw)
		line := pipCommentRe.ReplaceAllString(current.String(), "")
		current.Reset()
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// addPipTarget adds a requirement specifier: a named requirement, a local
// project or archive, or a URL (a download the project names itself, not a
// registry's).
func (p *Project) addPipTarget(install *pipInstall, target, source string) {
	switch {
	case strings.HasPrefix(target, "file:"):
		p.addPipLocal(install, strings.TrimPrefix(strings.TrimPrefix(target, "file:"), "//"))
	case pipDirectReferenceRe.MatchString(target):
		// name @ url: a local file reference is a local project makewand
		// does not inspect.
		_, reference, _ := strings.Cut(target, "@")
		if strings.HasPrefix(strings.TrimSpace(reference), "file:") {
			install.localCode = true
		}
	case strings.Contains(target, "://") || pipVCSRe.MatchString(target):
	case strings.HasPrefix(target, ".") || strings.HasPrefix(target, "~") || strings.ContainsAny(target, "/\\") || pipArchiveRe.MatchString(target):
		p.addPipLocal(install, target)
	default:
		if named, ok := pipNamedRequirementText(target); ok {
			install.downloadReqs = append(install.downloadReqs, named)
		}
		if requirement, ok := parsePipRequirement(target); ok {
			requirement.source = source
			install.requirements = append(install.requirements, requirement)
		}
	}
}

// addPipLocal adds a local project directory or archive. A wheel runs no code
// when it is installed; anything else does.
func (p *Project) addPipLocal(install *pipInstall, path string) {
	if strings.HasSuffix(strings.ToLower(path), ".whl") {
		return
	}
	install.localCode = true
	path = filepath.Clean(path)
	if path != "." {
		full, err := p.validatePath(path, false)
		if err != nil {
			return
		}
		if info, err := os.Stat(full); err != nil || !info.IsDir() {
			return
		}
	}
	install.projects = append(install.projects, path)
}

// parsePipRequirement parses a requirement that pip resolves from the index.
// One with environment markers (it may not apply) or a direct reference
// (name @ url) does not count.
func parsePipRequirement(text string) (pipRequirement, bool) {
	text = strings.Join(strings.Fields(text), " ")
	m := pipRequirementRe.FindStringSubmatch(text)
	if m == nil || strings.ContainsAny(m[2], ";@") {
		return pipRequirement{}, false
	}
	return pipRequirement{name: normalizePipName(m[1]), text: text}, true
}

// pipNamedRequirementText returns the text of a strict PEP 508 named
// requirement a `pip download` resolves from a package index: a name, optional
// extras and version specifiers, and an optional environment marker (pip
// evaluates markers itself and, under --only-binary=:all:, fetches only
// wheels). It is the only shape the networked fetch may safely be handed.
//
// Everything else is rejected, because pip builds it to read its metadata even
// under --only-binary=:all: - running project code while the network is up
// (confirmed on pip 24 and 26): a URL scheme, a VCS prefix (git+/hg+/svn+/bzr+),
// a file:, a path, and a direct reference (name @ url). The part before the
// marker must match pipNamedReqRe exactly (no ':', '/', '@' or space can
// smuggle a URL), and any '@' anywhere rejects a direct reference outright.
func pipNamedRequirementText(text string) (string, bool) {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" || strings.Contains(text, "@") {
		return "", false
	}
	head := text
	if i := strings.IndexByte(text, ';'); i >= 0 {
		head = strings.TrimSpace(text[:i]) // the environment marker follows; pip evaluates it
	}
	if !pipNamedReqRe.MatchString(head) {
		return "", false
	}
	return text, true
}

// normalizePipName normalizes a Python project name (PEP 503).
func normalizePipName(name string) string {
	return strings.ToLower(pipNameSeparatorsRe.ReplaceAllString(name, "-"))
}

// pipBuildRequirementNeed returns a build requirement of the local project in
// dir that pip installs into an isolated build environment, from the index
// whatever is installed already: the [build-system] requires of its
// pyproject.toml, or setuptools when pip uses the setuptools backend by
// default (a pyproject.toml without [build-system], or a setup.py project
// under --use-pep517). A setup.py project without --use-pep517 may take the
// legacy path older pip versions still have, which builds in place.
func (p *Project) pipBuildRequirementNeed(install pipInstall, dir string) string {
	if install.noIsolation {
		return ""
	}
	manifest := filepath.Join(dir, "pyproject.toml")
	requires := []string{"setuptools>=40.8.0"}
	if content, err := p.ReadFile(manifest); err == nil {
		doc := parseTOML(content)
		_, backend := doc.value("build-system", "build-backend")
		switch value, ok := doc.value("build-system", "requires"); {
		case install.noPEP517 && !backend:
			return ""
		case ok:
			requires = tomlStrings(value)
		case doc.hasTable("build-system"):
			return "" // pip rejects [build-system] without requires before building
		}
	} else {
		manifest = filepath.Join(dir, "setup.py")
		if _, err := p.ReadFile(manifest); err != nil || !install.pep517 {
			return ""
		}
	}
	for _, text := range requires {
		if requirement, ok := parsePipRequirement(text); ok {
			return requirement.text + " (build requirement for " + manifest + ")"
		}
	}
	return ""
}

// pipStaticDependencies returns the dependencies the pyproject.toml of the
// local project in dir declares statically ([project] dependencies that are
// not dynamic). Those a setup.py computes are unknown.
func (p *Project) pipStaticDependencies(dir string) []pipRequirement {
	manifest := filepath.Join(dir, "pyproject.toml")
	content, err := p.ReadFile(manifest)
	if err != nil {
		return nil
	}
	doc := parseTOML(content)
	if dynamic, ok := doc.value("project", "dynamic"); ok && slices.Contains(tomlStrings(dynamic), "dependencies") {
		return nil
	}
	value, ok := doc.value("project", "dependencies")
	if !ok {
		return nil
	}
	var requirements []pipRequirement
	for _, text := range tomlStrings(value) {
		if requirement, ok := parsePipRequirement(text); ok {
			requirement.source = "dependencies in " + manifest
			requirements = append(requirements, requirement)
		}
	}
	return requirements
}

// pipInstalledDistributions returns the normalized names of the distributions
// installed where the sandbox's pip finds them (pipSiteDirs).
func (p *Project) pipInstalledDistributions() map[string]bool {
	installed := map[string]bool{}
	for _, dir := range p.pipSiteDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			for _, suffix := range []string{".dist-info", ".egg-info", ".egg-link"} {
				if stem, ok := strings.CutSuffix(entry.Name(), suffix); ok {
					name, _, _ := strings.Cut(stem, "-")
					installed[normalizePipName(name)] = true
				}
			}
		}
	}
	return installed
}

// pipSiteDirs returns the site-packages and dist-packages directories of the
// interpreters that pip and python3 on the sandbox PATH run with (and of /usr
// and /usr/local), and the user site in the sandbox HOME (pip install
// --user).
func (p *Project) pipSiteDirs() []string {
	pathEnv := sandboxPathEnv()
	prefixes := []string{"/usr", "/usr/local"}
	for _, name := range []string{"pip", "pip3", "python3", "python"} {
		bin := lookPathIn(name, pathEnv)
		if bin == "" {
			continue
		}
		for _, exe := range []string{bin, scriptInterpreter(bin, pathEnv)} {
			if exe == "" {
				continue
			}
			prefixes = append(prefixes, filepath.Dir(filepath.Dir(exe)))
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				prefixes = append(prefixes, filepath.Dir(filepath.Dir(real)))
			}
		}
	}
	slices.Sort(prefixes)
	var patterns []string
	for _, prefix := range slices.Compact(prefixes) {
		patterns = append(patterns,
			filepath.Join(prefix, "lib", "python3*", "site-packages"),
			filepath.Join(prefix, "lib", "python3*", "dist-packages"),
			filepath.Join(prefix, "lib", "python3", "dist-packages"),
			filepath.Join(prefix, "lib64", "python3*", "site-packages"),
			filepath.Join(prefix, "local", "lib", "python3*", "dist-packages"))
	}
	if projectDir, err := p.execWorkingDir(); err == nil {
		patterns = append(patterns, filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath), ".local", "lib", "python3*", "site-packages"))
	}
	var dirs []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		dirs = append(dirs, matches...)
	}
	return dirs
}

// scriptInterpreter returns the Python interpreter the #! line of script
// names (through env too), or "".
func scriptInterpreter(script, pathEnv string) string {
	f, err := os.Open(script) //nolint:gosec // G304: an executable resolved from the sandbox PATH
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 256)
	n, _ := io.ReadFull(f, head)
	line, _, _ := strings.Cut(string(head[:n]), "\n")
	interpreter, ok := strings.CutPrefix(line, "#!")
	fields := strings.Fields(interpreter)
	if !ok || len(fields) == 0 {
		return ""
	}
	exe := fields[0]
	if filepath.Base(exe) == "env" && len(fields) > 1 {
		exe = lookPathIn(fields[1], pathEnv)
	}
	if !strings.HasPrefix(filepath.Base(exe), "python") {
		return ""
	}
	return exe
}

// pipConfigContents returns the pip configuration files the sandbox's pip
// reads: site-wide, and in the sandbox HOME.
func (p *Project) pipConfigContents() []string {
	var contents []string
	for _, file := range []string{"/etc/pip.conf", "/etc/xdg/pip/pip.conf"} {
		if data, err := os.ReadFile(file); err == nil { //nolint:gosec // G304: fixed pip configuration paths
			contents = append(contents, string(data))
		}
	}
	home := filepath.FromSlash(sandboxHomeRelPath)
	for _, file := range []string{filepath.Join(home, ".config", "pip", "pip.conf"), filepath.Join(home, ".pip", "pip.conf")} {
		if content, err := p.ReadFile(file); err == nil {
			contents = append(contents, content)
		}
	}
	return contents
}

// pipConfigKeyValues calls visit with the normalized key and trimmed value of
// each `key = value` / `key: value` line of the pip configuration.
func (p *Project) pipConfigKeyValues(visit func(key, value string)) {
	for _, content := range p.pipConfigContents() {
		for _, line := range strings.Split(content, "\n") {
			i := strings.IndexAny(line, "=:")
			if i < 0 {
				continue
			}
			visit(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(line[:i])), "_", "-"), strings.TrimSpace(line[i+1:]))
		}
	}
}

// pipConfigMayAvoidIndex reports whether a pip configuration file the
// sandbox's pip reads (site-wide, or in the sandbox HOME) sets no-index,
// find-links, a local index-url or no-build-isolation.
func (p *Project) pipConfigMayAvoidIndex() bool {
	avoids := false
	p.pipConfigKeyValues(func(key, value string) {
		switch key {
		case "no-index", "find-links", "no-build-isolation":
			avoids = true
		case "index-url", "extra-index-url":
			if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
				avoids = true
			}
		}
	})
	return avoids
}

// pipConfigSetsIndex reports whether a pip configuration file names any index
// or find-links at all, http(s) ones included: the fetch then did not use only
// the default PyPI index, so a nonexistent-project fact check against PyPI
// would be unsound.
func (p *Project) pipConfigSetsIndex() bool {
	set := false
	p.pipConfigKeyValues(func(key, _ string) {
		switch key {
		case "index-url", "extra-index-url", "find-links":
			set = true
		}
	})
	return set
}

// cargoDependency is a dependency a Cargo.toml declares.
type cargoDependency struct {
	name     string // the dependency's key in the manifest
	pkg      string // the package name, after a rename
	registry bool   // resolved from a registry (no path or git source)
	dev      bool   // a dev-dependency
}

// cargoLockPackage is a [[package]] entry of Cargo.lock.
type cargoLockPackage struct {
	name, version, source string
	dependencies          []string // "name", "name version" or "name version (source)"
}

func (pkg cargoLockPackage) fromRegistry() bool {
	return strings.HasPrefix(pkg.source, "registry+") || strings.HasPrefix(pkg.source, "sparse+")
}

// cargoCrateFileRe matches the characters of a crate name or version that may
// form a cache file name.
var cargoCrateFileRe = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

// cargoRegistryNeed returns a registry package the cargo plan has to download,
// or "": a dependency the manifests declare when there is no Cargo.lock (cargo
// then resolves it from the registry index) or one missing from Cargo.lock,
// and otherwise a Cargo.lock registry package whose crate is not in the
// sandbox's cargo cache (for `cargo build`, only those the build needs). It
// returns "" when cargo may get packages elsewhere or not at all: offline
// mode, a local source replacement or path overrides in a cargo
// configuration, [patch] or [replace] sections.
func (p *Project) cargoRegistryNeed(plan ExecPlan) string {
	if slices.Contains(plan.Args, "--offline") || slices.Contains(plan.Args, "--frozen") || p.cargoConfigMayAvoidRegistry() {
		return ""
	}
	manifest, err := p.ReadFile("Cargo.toml")
	if err != nil {
		return ""
	}
	deps, ok := p.cargoWorkspaceDependencies(manifest)
	if !ok {
		return ""
	}
	lock, err := p.ReadFile("Cargo.lock")
	if err != nil {
		for _, dep := range deps {
			if dep.registry {
				return dep.pkg + " (Cargo.toml; without a Cargo.lock cargo resolves it from the registry index)"
			}
		}
		return ""
	}
	packages := cargoLockPackages(lock)
	byName := map[string][]cargoLockPackage{}
	for _, pkg := range packages {
		byName[pkg.name] = append(byName[pkg.name], pkg)
	}
	var roots []string
	for _, dep := range deps {
		if dep.registry && len(byName[dep.pkg]) == 0 {
			return dep.pkg + " (Cargo.toml, not in Cargo.lock; cargo resolves it from the registry index)"
		}
		if !dep.dev {
			// Path dependencies too: what they depend on is built as well.
			roots = append(roots, dep.pkg)
		}
	}
	wanted := packages
	if len(plan.Args) > 0 && plan.Args[0] == "build" {
		wanted = cargoLockClosure(byName, roots)
	}
	projectDir, err := p.execWorkingDir()
	if err != nil {
		return ""
	}
	cache := filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath), ".cargo", "registry", "cache")
	for _, pkg := range wanted {
		if !pkg.fromRegistry() || !cargoCrateFileRe.MatchString(pkg.name) || !cargoCrateFileRe.MatchString(pkg.version) {
			continue
		}
		if matches, _ := filepath.Glob(filepath.Join(cache, "*", pkg.name+"-"+pkg.version+".crate")); len(matches) == 0 {
			return pkg.name + " " + pkg.version + " (Cargo.lock; not in the sandbox's cargo cache)"
		}
	}
	return ""
}

// cargoLockClosure returns the Cargo.lock packages named in roots and every
// package they depend on.
func cargoLockClosure(byName map[string][]cargoLockPackage, roots []string) []cargoLockPackage {
	var closure []cargoLockPackage
	seen := map[string]bool{}
	queue := slices.Clone(roots)
	for len(queue) > 0 {
		fields := strings.Fields(queue[0])
		queue = queue[1:]
		if len(fields) == 0 {
			continue
		}
		for _, pkg := range byName[fields[0]] {
			if (len(fields) > 1 && pkg.version != fields[1]) || seen[pkg.name+" "+pkg.version] {
				continue
			}
			seen[pkg.name+" "+pkg.version] = true
			closure = append(closure, pkg)
			queue = append(queue, pkg.dependencies...)
		}
	}
	return closure
}

// cargoLockPackages returns the [[package]] entries of a Cargo.lock.
func cargoLockPackages(lock string) []cargoLockPackage {
	var packages []cargoLockPackage
	doc := parseTOML(lock)
	for _, entry := range doc.entries {
		if entry.table != "package" || entry.index == 0 {
			continue
		}
		for len(packages) < entry.index {
			packages = append(packages, cargoLockPackage{})
		}
		pkg := &packages[entry.index-1]
		switch entry.key {
		case "name":
			pkg.name = firstTOMLString(entry.value)
		case "version":
			pkg.version = firstTOMLString(entry.value)
		case "source":
			pkg.source = firstTOMLString(entry.value)
		case "dependencies":
			pkg.dependencies = tomlStrings(entry.value)
		}
	}
	return packages
}

// cargoWorkspaceDependencies returns the dependencies the root Cargo.toml and
// its workspace members declare, or false when the root has [patch] or
// [replace] sections.
func (p *Project) cargoWorkspaceDependencies(manifest string) ([]cargoDependency, bool) {
	root := parseTOML(manifest)
	if root.hasTable("replace") || root.hasTablePrefix("patch") {
		return nil, false
	}
	workspace := map[string]cargoDependency{}
	for _, dep := range cargoManifestDependencies(root, "workspace.dependencies", nil) {
		workspace[dep.name] = dep
	}
	deps := cargoManifestDependencies(root, "", workspace)
	projectDir, err := p.execWorkingDir()
	if err != nil {
		return deps, true
	}
	value, _ := root.value("workspace", "members")
	members := tomlStrings(value)
	read := 0
	for _, pattern := range members[:min(len(members), 256)] {
		full, err := p.validatePath(pattern, false)
		if err != nil {
			continue
		}
		matches, _ := filepath.Glob(full)
		for _, match := range matches {
			rel, err := filepath.Rel(projectDir, match)
			if err != nil || read >= 64 {
				continue
			}
			if content, err := p.ReadFile(filepath.Join(rel, "Cargo.toml")); err == nil {
				read++
				deps = append(deps, cargoManifestDependencies(parseTOML(content), "", workspace)...)
			}
		}
		if read >= 64 {
			break
		}
	}
	return deps, true
}

// cargoManifestDependencies returns the dependencies a Cargo.toml declares:
// in its dependency tables ([dependencies], [dev-dependencies],
// [build-dependencies], target-specific ones and [dependencies.<name>]
// tables), or, when only names one, in that table alone. Inherited ones
// (workspace = true) take their source from workspace.
func cargoManifestDependencies(doc tomlDocument, only string, workspace map[string]cargoDependency) []cargoDependency {
	type spec struct {
		name  string
		attrs map[string]string
		dev   bool
	}
	specs := map[string]*spec{}
	var order []string
	for _, entry := range doc.entries {
		key := entry.key
		table := entry.table
		if i := strings.LastIndex(table, "dependencies."); i >= 0 {
			// [dependencies.serde]: the table names the dependency.
			key = table[i+len("dependencies."):] + "." + key
			table = table[:i+len("dependencies")]
		}
		if !strings.HasSuffix(table, "dependencies") || (only != "" && table != only) || (only == "" && table == "workspace.dependencies") {
			continue
		}
		name, attr, _ := strings.Cut(key, ".")
		id := table + "\x00" + name
		s := specs[id]
		if s == nil {
			s = &spec{name: name, attrs: map[string]string{}, dev: strings.Contains(table, "dev-dependencies") || strings.Contains(table, "dev_dependencies")}
			specs[id] = s
			order = append(order, id)
		}
		switch {
		case attr != "":
			s.attrs[attr] = entry.value
		case strings.HasPrefix(entry.value, "{"):
			maps.Copy(s.attrs, tomlInlineTable(entry.value))
		default:
			s.attrs["version"] = entry.value
		}
	}
	var deps []cargoDependency
	for _, id := range order {
		s := specs[id]
		dep := cargoDependency{name: s.name, pkg: s.name, dev: s.dev, registry: true}
		if pkg := firstTOMLString(s.attrs["package"]); pkg != "" {
			dep.pkg = pkg
		}
		_, path := s.attrs["path"]
		_, git := s.attrs["git"]
		switch {
		case path || git:
			dep.registry = false
		case strings.TrimSpace(s.attrs["workspace"]) == "true":
			inherited, ok := workspace[dep.name]
			dep.pkg, dep.registry = inherited.pkg, ok && inherited.registry
		}
		if dep.pkg != "" {
			deps = append(deps, dep)
		}
	}
	return deps
}

// cargoConfigMayAvoidRegistry reports whether a cargo configuration file of
// the project or of the sandbox's CARGO_HOME lets cargo do without the
// registry: offline mode, a local source replacement (a directory or a local
// registry) or path overrides.
func (p *Project) cargoConfigMayAvoidRegistry() bool {
	home := filepath.Join(filepath.FromSlash(sandboxHomeRelPath), ".cargo")
	for _, file := range []string{filepath.Join(".cargo", "config.toml"), filepath.Join(".cargo", "config"), filepath.Join(home, "config.toml"), filepath.Join(home, "config")} {
		content, err := p.ReadFile(file)
		if err != nil {
			continue
		}
		for _, entry := range parseTOML(content).entries {
			key := entry.fullKey()
			switch {
			case key == "net.offline" && strings.TrimSpace(entry.value) == "true", key == "paths":
				return true
			case strings.HasPrefix(key, "source.") && (strings.HasSuffix(key, ".directory") || strings.HasSuffix(key, ".local-registry")):
				return true
			}
		}
	}
	return false
}

// tomlDocument is what the small TOML reader below sees of a document: its
// table headers, and each key/value line with the table it belongs to. It
// covers what makewand reads from Cargo.toml, Cargo.lock, pyproject.toml and
// cargo configuration: dotted keys, quoted header parts, strings, inline
// tables and arrays (multi-line arrays joined). Multi-line strings are not
// interpreted.
type tomlDocument struct {
	tables  map[string]bool
	entries []tomlEntry
}

// tomlEntry is a key/value line: the header of its table without spaces (""
// before any header), the occurrence of that header when it heads an array of
// tables ([[package]], counted from 1), its key without quotes, and its raw
// value.
type tomlEntry struct {
	table string
	index int
	key   string
	value string
}

// fullKey returns the entry's key with its table in front.
func (e tomlEntry) fullKey() string {
	if e.table == "" {
		return e.key
	}
	return e.table + "." + e.key
}

// parseTOML reads a TOML document (see tomlDocument).
func parseTOML(content string) tomlDocument {
	doc := tomlDocument{tables: map[string]bool{}}
	table, index := "", 0
	occurrences := map[string]int{}
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(tomlStripComment(lines[i]))
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "[["):
			table = tomlHeader(strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]"))
			occurrences[table]++
			index = occurrences[table]
			doc.tables[table] = true
			continue
		case strings.HasPrefix(line, "["):
			table, index = tomlHeader(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")), 0
			doc.tables[table] = true
			continue
		}
		eq := tomlIndexOutsideStrings(line, '=')
		if eq < 0 {
			continue
		}
		value := strings.TrimSpace(line[eq+1:])
		for open := tomlOpenBrackets(value); open > 0 && i+1 < len(lines); {
			i++
			next := strings.TrimSpace(tomlStripComment(lines[i]))
			open += tomlOpenBrackets(next)
			value += " " + next
		}
		doc.entries = append(doc.entries, tomlEntry{table: table, index: index, key: tomlKey(line[:eq]), value: value})
	}
	return doc
}

// value returns the raw value of key in table, set in the table itself or as
// a dotted key.
func (d tomlDocument) value(table, key string) (string, bool) {
	want := table + "." + key
	for i := len(d.entries) - 1; i >= 0; i-- {
		if d.entries[i].fullKey() == want {
			return d.entries[i].value, true
		}
	}
	return "", false
}

// hasTable reports whether the document has table, as a header or through a
// dotted key.
func (d tomlDocument) hasTable(table string) bool {
	if d.tables[table] {
		return true
	}
	for _, entry := range d.entries {
		if strings.HasPrefix(entry.fullKey(), table+".") {
			return true
		}
	}
	return false
}

// hasTablePrefix reports whether the document has prefix or a table below it.
func (d tomlDocument) hasTablePrefix(prefix string) bool {
	if d.hasTable(prefix) {
		return true
	}
	for table := range d.tables {
		if strings.HasPrefix(table, prefix+".") {
			return true
		}
	}
	return false
}

// tomlScan calls visit for each byte of s outside strings, with the bracket
// depth before it, and stops when visit returns false.
func tomlScan(s string, visit func(i, depth int) bool) {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		case c == '"' || c == '\'':
			quote = c
			continue
		}
		if !visit(i, depth) {
			return
		}
		switch c {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
}

// tomlIndexOutsideStrings returns the index of the first c outside strings
// and brackets, or -1.
func tomlIndexOutsideStrings(s string, c byte) int {
	at := -1
	tomlScan(s, func(i, depth int) bool {
		if s[i] == c && depth == 0 {
			at = i
			return false
		}
		return true
	})
	return at
}

// tomlStripComment removes a comment from a line.
func tomlStripComment(line string) string {
	end := len(line)
	tomlScan(line, func(i, _ int) bool {
		if line[i] == '#' {
			end = i
			return false
		}
		return true
	})
	return line[:end]
}

// tomlOpenBrackets returns how many brackets and braces s leaves open.
func tomlOpenBrackets(s string) int {
	open := 0
	tomlScan(s, func(i, depth int) bool {
		open = depth
		switch s[i] {
		case '[', '{':
			open++
		case ']', '}':
			open--
		}
		return true
	})
	return open
}

// tomlHeader normalizes a table header: whitespace outside quotes removed.
func tomlHeader(raw string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		case quote == 0 && (c == ' ' || c == '\t'):
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// tomlKey normalizes a key: quotes and whitespace removed.
func tomlKey(raw string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\'' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, raw)
}

// tomlStrings returns the strings of a raw TOML value: the elements of an
// array of strings, or a single string.
func tomlStrings(value string) []string {
	var out []string
	for i := 0; i < len(value); i++ {
		quote := value[i]
		if quote != '"' && quote != '\'' {
			continue
		}
		var b strings.Builder
		j := i + 1
		for ; j < len(value) && value[j] != quote; j++ {
			if quote == '"' && value[j] == '\\' && j+1 < len(value) {
				j++
			}
			b.WriteByte(value[j])
		}
		out = append(out, b.String())
		i = j
	}
	return out
}

// firstTOMLString returns the first string of a raw TOML value, or "".
func firstTOMLString(value string) string {
	if values := tomlStrings(value); len(values) > 0 {
		return values[0]
	}
	return ""
}

// tomlInlineTable returns the keys and raw values of a TOML inline table.
func tomlInlineTable(value string) map[string]string {
	value = strings.TrimSpace(value)
	fields := map[string]string{}
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return fields
	}
	body := value[1 : len(value)-1]
	start := 0
	tomlScan(body+",", func(i, depth int) bool {
		if depth == 0 && (i == len(body) || body[i] == ',') {
			part := body[start:i]
			if eq := tomlIndexOutsideStrings(part, '='); eq >= 0 {
				fields[tomlKey(part[:eq])] = strings.TrimSpace(part[eq+1:])
			}
			start = i + 1
		}
		return true
	})
	return fields
}

// sandboxPathEnv is the PATH the verification sandbox uses.
func sandboxPathEnv() string {
	pathEnv := strings.TrimSpace(verifyGetenv("PATH"))
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return pathEnv
}

// isBwrapSetupFailure reports whether a sandboxed run failed inside bubblewrap
// setup (mounts, namespaces) before the wrapped command executed.
func isBwrapSetupFailure(result *ExecResult) bool {
	if result == nil || result.ExitCode != 1 || strings.TrimSpace(result.Stdout) != "" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(result.Stderr), "bwrap: ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// DetectInstallPlan detects the dependency install command for the current project.
func (p *Project) DetectInstallPlan() (*ExecPlan, error) {
	candidates := []struct {
		file    string
		command string
		args    []string
	}{
		{"package.json", "npm", []string{"install", "--ignore-scripts"}},
		{"requirements.txt", "pip", []string{"install", "--user", "-r", "requirements.txt"}},
		{"pyproject.toml", "pip", []string{"install", "--user", "-e", "."}},
		{"go.mod", "go", []string{"mod", "tidy"}},
		{"Cargo.toml", "cargo", []string{"build"}},
	}

	for _, c := range candidates {
		content, err := p.ReadFile(c.file)
		if err == nil && strings.TrimSpace(content) != "" {
			return &ExecPlan{
				Kind:     "deps",
				Detector: c.file,
				Command:  c.command,
				Args:     append([]string(nil), c.args...),
			}, nil
		}
	}
	return nil, nil
}

// DetectTestPlan detects the test execution command for the current project.
func (p *Project) DetectTestPlan() (*ExecPlan, error) {
	if content, err := p.ReadFile("package.json"); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal([]byte(content), &pkg) == nil {
			if _, ok := pkg.Scripts["test"]; ok {
				return &ExecPlan{
					Kind:     "tests",
					Detector: "package.json",
					Command:  "npm",
					Args:     []string{"test"},
				}, nil
			}
		}
	}
	// Explicit pytest configuration keeps its historical priority over go.mod
	// and Cargo.toml (it is a declared test runner).
	if detector := p.pytestConfigDetector(); detector != "" {
		return &ExecPlan{
			Kind:     "tests",
			Detector: detector,
			Command:  "pytest",
			Args:     nil,
		}, nil
	}
	if _, err := p.ReadFile("go.mod"); err == nil {
		return &ExecPlan{
			Kind:     "tests",
			Detector: "go.mod",
			Command:  "go",
			Args:     []string{"test", "./..."},
		}, nil
	}
	if _, err := p.ReadFile("Cargo.toml"); err == nil {
		return &ExecPlan{
			Kind:     "tests",
			Detector: "Cargo.toml",
			Command:  "cargo",
			Args:     []string{"test"},
		}, nil
	}
	// Convention-only Python suites (conftest.py, test_*.py, *_test.py) rank
	// below every declared runner so mixed repositories keep their runner.
	if detector := p.pytestFileDetector(); detector != "" {
		return &ExecPlan{
			Kind:     "tests",
			Detector: detector,
			Command:  "pytest",
			Args:     nil,
		}, nil
	}
	return nil, nil
}

// pytestConfigDetector returns the file that declares a pytest configuration:
// pytest.ini, pyproject.toml [tool.pytest...], setup.cfg [tool:pytest] or
// tox.ini [pytest]. It returns "" when none does.
func (p *Project) pytestConfigDetector() string {
	if _, err := p.ReadFile("pytest.ini"); err == nil {
		return "pytest.ini"
	}
	for _, c := range []struct {
		file     string
		sections []string
	}{
		{"pyproject.toml", []string{"[tool.pytest.ini_options]", "[tool.pytest]"}},
		{"setup.cfg", []string{"[tool:pytest]"}},
		{"tox.ini", []string{"[pytest]"}},
	} {
		content, err := p.ReadFile(c.file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			for _, section := range c.sections {
				if strings.EqualFold(line, section) {
					return c.file
				}
			}
		}
	}
	return ""
}

// pytestFileDetector finds a pytest suite by convention: a conftest.py or a
// test_*.py / *_test.py module, searched breadth-first over a bounded part of
// the tree (ignored directories such as .venv and node_modules are skipped).
func (p *Project) pytestFileDetector() string {
	const (
		maxDepth   = 4
		maxEntries = 5000
	)
	root, err := p.execWorkingDir()
	if err != nil {
		return ""
	}
	type dirItem struct {
		rel   string
		depth int
	}
	queue := []dirItem{{rel: ".", depth: 0}}
	seen := 0
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(filepath.Join(root, item.rel))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			seen++
			if seen > maxEntries {
				return ""
			}
			name := entry.Name()
			rel := filepath.ToSlash(filepath.Join(item.rel, name))
			if entry.IsDir() {
				if item.depth+1 < maxDepth && !shouldIgnoreSet[name] && !strings.HasPrefix(name, ".") && name != "site-packages" {
					queue = append(queue, dirItem{rel: rel, depth: item.depth + 1})
				}
				continue
			}
			if !entry.Type().IsRegular() {
				continue
			}
			lower := strings.ToLower(name)
			if lower == "conftest.py" ||
				(strings.HasPrefix(lower, "test_") && strings.HasSuffix(lower, ".py")) ||
				(strings.HasSuffix(lower, "_test.py") && lower != "_test.py") {
				return rel
			}
		}
	}
	return ""
}

// InstallDeps detects the project type and installs dependencies.
func (p *Project) InstallDeps(ctx context.Context) (*ExecResult, error) {
	plan, err := p.DetectInstallPlan()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &ExecResult{Stdout: "No package manager detected"}, nil
	}
	return p.RunRestrictedPlan(ctx, *plan)
}

// RunTests detects and runs the project's test suite.
func (p *Project) RunTests(ctx context.Context) (*ExecResult, error) {
	plan, err := p.DetectTestPlan()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &ExecResult{Stdout: "No test framework detected"}, nil
	}
	return p.RunRestrictedPlan(ctx, *plan)
}

const maxOutputBytes = processjob.MaxOutputBytes

// RestrictedPlanTimeout bounds a single project command (dependency install,
// test run, verification step). Callers that impose their own deadline (the
// TUI build flow) use this same value, so the engine never kills a command
// earlier than the caller's budget allows.
const RestrictedPlanTimeout = 10 * time.Minute

// restrictedExecContext derives the execution context for one command: the
// caller's deadline when it is sooner, otherwise RestrictedPlanTimeout.
func restrictedExecContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, RestrictedPlanTimeout)
}

func (p *Project) execWithPolicy(ctx context.Context, command string, args []string, policy execPolicy) (*ExecResult, error) {
	return p.execWithPolicyLaunch(ctx, command, args, policy, nil)
}

// execWithPolicyLaunch is execWithPolicy with an optional user-mode network
// launch: its descriptors are handed to the sandbox and the network helper is
// attached after start, before the held command runs. When the helper cannot
// be attached the sandbox is killed instead (userNet.attached is false).
func (p *Project) execWithPolicyLaunch(ctx context.Context, command string, args []string, policy execPolicy, userNet *userNetLaunch) (*ExecResult, error) {
	if policy.allowCommandPath {
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("empty command")
		}
	} else if err := validateCommandName(command); err != nil {
		return nil, err
	}
	if !policy.allowAnyCommand {
		if _, ok := policy.allowedCommands[command]; !ok {
			return nil, fmt.Errorf("command %q is blocked by execution policy", command)
		}
	}

	projectDir, err := p.execWorkingDir()
	if err != nil {
		return nil, err
	}

	ctx, cancel := restrictedExecContext(ctx)
	defer cancel()

	// #nosec G702 -- argv bypasses the shell; names/allowlist are checked above, and executable paths require the explicit internal policy.
	cmd := exec.Command(command, args...)
	cmd.Dir = projectDir
	if policy.stripSensitive {
		cmd.Env = sanitizeExecEnv(cmd.Environ())
	}
	if len(policy.env) > 0 {
		cmd.Env = append(cmd.Environ(), policy.env...)
	}
	if filepath.Base(command) == "git" {
		baseEnv := cmd.Env
		if baseEnv == nil {
			baseEnv = cmd.Environ()
		}
		cmd.Env = sanitizeGitExecEnv(baseEnv)
	}
	setProcessGroup(cmd)

	capture := processjob.NewCapture(maxOutputBytes, func() { killProcessGroup(cmd) })
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()

	startProcess := processjob.Start
	closeSeccomp := func() {}
	if policy.sandboxed {
		startProcess = processjob.StartSandboxed
		if policy.allowCommandPath {
			if closeSeccomp, err = attachSandboxSeccomp(cmd, args); err != nil {
				return nil, err
			}
		}
	}
	if userNet != nil {
		if err := userNet.attachFiles(cmd); err != nil {
			closeSeccomp()
			return nil, err
		}
	}
	start := time.Now()
	cleanupProcess, err := startProcess(cmd)
	closeSeccomp()
	if userNet != nil {
		userNet.closeChildEnds()
	}
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, &execution.UnknownOutcomeError{Err: fmt.Errorf("exec %s interrupted: %w", command, errors.Join(contextErr, err))}
		}
		return nil, fmt.Errorf("exec %s: %w", command, err)
	}
	defer cleanupProcess()

	// Kill the entire process group when the context deadline is exceeded.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				killProcessGroup(cmd)
			}
		case <-watchDone:
		}
	}()

	if userNet != nil {
		// The sandbox holds the command until the network is attached; the
		// helper is stopped once the command exits. When attaching fails the
		// sandbox must not go on to run the command without the network it
		// was launched for (it would spend minutes in registry retries):
		// connect stops the held sandbox, and killing the process group makes
		// sure nothing it started survives. The caller decides how to run the
		// step instead.
		userNet.connect(ctx)
		if !userNet.attached {
			killProcessGroup(cmd)
		}
		defer userNet.release()
	}
	err = cmd.Wait()
	close(watchDone)
	killProcessGroup(cmd)
	duration := time.Since(start)

	stdoutDropped, stderrDropped := capture.DroppedBytes()
	result := &ExecResult{
		Stdout:             capture.StdoutString(),
		Stderr:             capture.StderrString(),
		Duration:           duration,
		StdoutDroppedBytes: stdoutDropped,
		StderrDroppedBytes: stderrDropped,
	}
	if capture.Exceeded() {
		result.ExitCode = -1
		return result, &execution.UnknownOutcomeError{Err: processjob.OutputLimitError()}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.ExitCode = -1
		return result, &execution.UnknownOutcomeError{Err: fmt.Errorf("exec %s interrupted: %w", command, ctxErr)}
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		result.ExitCode = -1
		return result, &execution.UnknownOutcomeError{Err: fmt.Errorf("exec %s inherited output pipes did not close: %w", command, err)}
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("exec %s: %w", command, err)
		}
	}

	return result, nil
}

func (p *Project) execWorkingDir() (string, error) {
	if p == nil || p.Path == "" {
		return "", fmt.Errorf("project path is empty")
	}
	abs, err := filepath.Abs(p.Path)
	if err != nil {
		return "", fmt.Errorf("resolve project path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("project path not accessible: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project path is not a directory: %s", abs)
	}
	return abs, nil
}

func validateCommandName(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("empty command")
	}
	if strings.Contains(command, "/") || strings.Contains(command, "\\") {
		return fmt.Errorf("command must be a plain executable name: %q", command)
	}
	return nil
}

func sanitizeExecEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		key := entry[:eq]
		if isSensitiveEnv(key) {
			continue
		}
		out = append(out, entry)
	}
	if !hasEnvKey(out, "NO_COLOR") {
		out = append(out, "NO_COLOR=1")
	}
	if !hasEnvKey(out, "MAKEWAND_SANDBOX") {
		out = append(out, "MAKEWAND_SANDBOX=1")
	}
	return out
}

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func isSensitiveEnv(key string) bool {
	if _, ok := sensitiveEnvExact[key]; ok {
		return true
	}
	return slices.ContainsFunc(sensitiveEnvSuffixes, func(s string) bool { return strings.HasSuffix(key, s) })
}

var dangerousGitEnvs = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_HOOKS_PATH":                   true,
	"GIT_EXEC_PATH":                    true,
	"GIT_EXTERNAL_DIFF":                true,
	"GIT_DIFF_OPTS":                    true,
	"GIT_PAGER":                        true,
	"GIT_EDITOR":                       true,
	"GIT_SEQUENCE_EDITOR":              true,
	"GIT_SSH":                          true,
	"GIT_SSH_COMMAND":                  true,
	"GIT_ASKPASS":                      true,
}

func isDangerousGitEnv(key string) bool {
	if dangerousGitEnvs[key] {
		return true
	}
	return strings.HasPrefix(key, "GIT_CONFIG_")
}

func sanitizeGitExecEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		key := entry[:eq]
		if isDangerousGitEnv(key) || key == "GIT_OPTIONAL_LOCKS" {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "GIT_OPTIONAL_LOCKS=0")
	return out
}

func quoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.ContainsAny(arg, " \t\n\"'`$&|;<>()[]{}*?!") {
		return strconv.Quote(arg)
	}
	return arg
}
