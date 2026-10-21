//go:build linux

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GEC-1021-1 round 2 (muse review of PR #91): close the residual ways project
// input could still run project code while the networked dependency fetch has
// the network, tighten the fetch's masks and index/constraint handling, and
// keep a candidate's own broken build/dependency a candidate failure.

// A buildable pip target in pyproject.toml - a VCS or URL reference in
// [build-system] requires or [project] dependencies - must never reach the
// networked `pip download`: pip builds it to read its metadata even under
// --only-binary=:all: (confirmed on pip 24 and 26), running project code with
// the network. The strict named-requirement parser rejects every such shape.
func TestPipNamedRequirementStrict_GEC1021R2(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		// Accepted: plain named requirements, with extras, specifiers, markers.
		{"flask", true},
		{"flask==2.0", true},
		{"requests[security]>=2,<3", true},
		{"hatchling>=1", true},
		{`six==1.16.0 ; python_version < "3.0"`, true},
		{"ruamel.yaml", true},
		// Rejected: the pyproject bypass forms and other buildable targets.
		{"git+https://attacker.example/r.git#egg=pwn", false},
		{"https://attacker.example/pwn-1.0.tar.gz", false},
		{"http://attacker.example/p.whl", false},
		{"evil @ file:///tmp/evil", false},
		{"pkg @ https://attacker.example/p.tgz", false},
		{"file:///tmp/evil", false},
		{"./local", false},
		{"../local", false},
		{"/abs/path", false},
		{"hg+https://h/r", false},
		{"", false},
		{"-e", false},
		{"--index-url", false},
	} {
		if _, ok := pipNamedRequirementText(tc.text); ok != tc.want {
			t.Errorf("pipNamedRequirementText(%q) = %v, want %v", tc.text, ok, tc.want)
		}
	}
}

// The two pyproject paths (build-system requires, project dependencies) are
// filtered by the same strict parser, so a VCS/URL entry there is never handed
// to the fetch while its named siblings are.
func TestPipPyprojectBuildableTargetsExcluded_GEC1021R2(t *testing.T) {
	project := newNeedProject(t, "pyproject.toml",
		"[build-system]\nrequires = [\"evil @ file:///tmp/evil\", \"git+https://a.example/b.git#egg=b\", \"hatchling>=1\"]\n"+
			"build-backend = \"hatchling.build\"\n\n[project]\nname = \"x\"\nversion = \"0\"\n"+
			"dependencies = [\"https://a.example/p.tar.gz\", \"requests>=2\"]\n")
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-e", "."}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fetch == nil {
		t.Fatal("fetch = nil, want a download for the named entries")
	}
	if !slices.Contains(fetch.Args, "hatchling>=1") || !slices.Contains(fetch.Args, "requests>=2") {
		t.Fatalf("fetch = %v, want the named build requirement and dependency", fetch.Args)
	}
	for _, arg := range fetch.Args {
		if strings.Contains(arg, "://") || strings.Contains(arg, "@") || strings.HasPrefix(arg, "git+") {
			t.Fatalf("fetch must not carry a buildable target, got %q in %v", arg, fetch.Args)
		}
	}
}

// The fetch resolves from the same indexes and version pins the install would:
// http(s) index options pass through (a non-http one is dropped), and a
// constraint file is reduced to its named pins in a sanitised file.
func TestPipFetchCarriesIndexOptionsAndConstraints_GEC1021R2(t *testing.T) {
	project := newNeedProject(t,
		"requirements.txt", "flask==2.0\n",
		"constraints.txt", "flask==2.0\nitsdangerous==2.1.2\n.\n./local\n")
	plan := ExecPlan{Kind: "deps", Command: "pip", Args: []string{
		"install", "-r", "requirements.txt", "-c", "constraints.txt",
		"-i", "https://mirror.example/simple/",
		"--extra-index-url", "https://extra.example/",
		"--trusted-host", "mirror.example",
		"--find-links", "https://fl.example/wheels/",
		"--find-links", "/local/wheels",
		"--index-url", "file:///opt/pypi",
	}}
	fetch, _, cleanup, err := project.planDependencyStages(plan, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fetch == nil {
		t.Fatal("fetch = nil, want a wheel download")
	}
	joined := strings.Join(fetch.Args, " ")
	for _, want := range []string{
		"--index-url https://mirror.example/simple/",
		"--extra-index-url https://extra.example/",
		"--trusted-host mirror.example",
		"--find-links https://fl.example/wheels/",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("fetch args %q, want %q", joined, want)
		}
	}
	if strings.Contains(joined, "/local/wheels") || strings.Contains(joined, "file:///opt/pypi") {
		t.Fatalf("fetch args %q must drop non-http(s) index sources", joined)
	}
	// The sanitised constraints file holds the named pins only, never a path.
	ci := slices.Index(fetch.Args, "--constraint")
	if ci < 0 || ci+1 >= len(fetch.Args) {
		t.Fatalf("fetch args %v, want a --constraint file", fetch.Args)
	}
	content, err := os.ReadFile(fetch.Args[ci+1])
	if err != nil {
		t.Fatalf("read sanitised constraints: %v", err)
	}
	if !strings.Contains(string(content), "flask==2.0") || !strings.Contains(string(content), "itsdangerous==2.1.2") {
		t.Fatalf("constraints = %q, want the named pins", content)
	}
	if strings.Contains(string(content), ".\n") && !strings.Contains(string(content), "==") {
		t.Fatalf("constraints = %q, want no buildable targets", content)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if line == "." || strings.HasPrefix(line, "./") {
			t.Fatalf("constraints must not carry a buildable target: %q", line)
		}
	}
}

// npm classification (unit): only a registry spec (a version, range, tag or an
// npm: alias of one) is fetched from the registry. Round 5 (D2R3) widened the
// refusal to every other source, http(s) tarballs included, as the round-2
// decision lists them: an https URL can be a git dependency to npm
// (https://github.com/user/repo), and none of them is a registry download. A
// workspace link in a real workspaces setup is still not refused here (npm
// rejects the protocol without fetching anything).
func TestNpmSpecRunsCodeOnFetch_GEC1021R2(t *testing.T) {
	for _, tc := range []struct {
		spec string
		ws   bool
		want bool
	}{
		{"^1.3.0", false, false},
		{"1.2.3", false, false},
		{"latest", false, false},
		{"npm:left-pad@1.3.0", false, false},
		{"https://example.com/p.tgz", false, true},
		{"http://example.com/p.tgz", false, true},
		{"file:../dep", false, true},
		{"link:../dep", false, true},
		{"git+https://example.com/r.git", false, true},
		{"git://example.com/r.git", false, true},
		{"github:user/repo", false, true},
		{"gitlab:user/repo", false, true},
		{"bitbucket:user/repo", false, true},
		{"user/repo", false, true},
		{"user/repo#semver:^1.2.3", false, true},
		{"./local", false, true},
		{"workspace:*", false, true},
		{"workspace:*", true, false},
	} {
		if got := npmNonRegistrySpec(tc.spec, tc.ws); got != tc.want {
			t.Errorf("npmNonRegistrySpec(%q, ws=%v) = %v, want %v", tc.spec, tc.ws, got, tc.want)
		}
	}
}

// npm non-registry detection reads package.json and the lockfile in use.
func TestNpmNonRegistryDependency_GEC1021R2(t *testing.T) {
	t.Run("package.json local dependency", func(t *testing.T) {
		p := newNeedProject(t, "package.json", `{"name":"x","dependencies":{"dep":"file:./dep"}}`)
		if got := p.npmNonRegistryDependency(); !strings.Contains(got, "dep") {
			t.Fatalf("npmNonRegistryDependency = %q, want the file: dependency", got)
		}
	})
	t.Run("registry-only tree", func(t *testing.T) {
		p := newNeedProject(t, "package.json", `{"name":"x","dependencies":{"left-pad":"^1.3.0"}}`)
		if got := p.npmNonRegistryDependency(); got != "" {
			t.Fatalf("npmNonRegistryDependency = %q, want none (registry only)", got)
		}
	})
	t.Run("http(s) tarball", func(t *testing.T) {
		// Refused since round 5: not a registry download (see above).
		p := newNeedProject(t, "package.json", `{"name":"x","dependencies":{"left-pad":"^1.3.0","tar":"https://host/tar.tgz"}}`)
		if got := p.npmNonRegistryDependency(); !strings.Contains(got, "tar") {
			t.Fatalf("npmNonRegistryDependency = %q, want the tarball dependency", got)
		}
	})
	t.Run("transitive git dependency in the lockfile", func(t *testing.T) {
		p := newNeedProject(t, "package.json", `{"name":"x","dependencies":{"left-pad":"^1.3.0"}}`,
			"package-lock.json", `{"packages":{"node_modules/left-pad":{"resolved":"git+ssh://git@h/r.git"}}}`)
		if got := p.npmNonRegistryDependency(); !strings.Contains(got, "left-pad") {
			t.Fatalf("npmNonRegistryDependency = %q, want the git-resolved lockfile entry", got)
		}
	})
	t.Run("workspace member with pnpm-workspace.yaml", func(t *testing.T) {
		p := newNeedProject(t, "package.json", `{"name":"x","dependencies":{"inner":"workspace:*"}}`,
			"pnpm-workspace.yaml", "packages:\n  - 'pkgs/*'\n")
		if got := p.npmNonRegistryDependency(); got != "" {
			t.Fatalf("npmNonRegistryDependency = %q, want none (real workspaces setup)", got)
		}
	})
}

// The npm fetch is hardened against lifecycle scripts by config too, not only
// the --ignore-scripts argument.
func TestNpmFetchSetsIgnoreScriptsConfig_GEC1021R2(t *testing.T) {
	project := newNeedProject(t, "package.json", `{"name":"x","dependencies":{"left-pad":"^1.3.0"}}`)
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if fetch == nil || !slices.Contains(fetch.env, "npm_config_ignore_scripts=true") {
		t.Fatalf("npm fetch env = %v, want npm_config_ignore_scripts=true", fetchEnvOf(fetch))
	}
}

// The networked `cargo fetch` is hardened against the executables a workspace
// .cargo/config.toml can name: RUSTC is pinned, the rustc-wrapper variables are
// cleared, the credential provider is forced to cargo's built-in token
// provider, and CARGO_HOME is the sandbox's own.
func TestCargoFetchEnvHardened_GEC1021R2(t *testing.T) {
	project := newNeedProject(t, "Cargo.toml", "[package]\nname=\"x\"\n\n[dependencies]\nserde = \"1\"\n")
	fetch, _, _, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"build"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fetch == nil || fetch.Command != "cargo" || len(fetch.Args) == 0 || fetch.Args[0] != "fetch" {
		t.Fatalf("cargo fetch stage = %+v, want `cargo fetch`", fetch)
	}
	env := fetch.env
	for _, want := range []string{
		"RUSTC_WRAPPER=",
		"RUSTC_WORKSPACE_WRAPPER=",
		"CARGO_BUILD_RUSTC_WRAPPER=",
		"CARGO_REGISTRY_GLOBAL_CREDENTIAL_PROVIDERS=cargo:token",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("cargo fetch env = %v, want %q", env, want)
		}
	}
	if !slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "CARGO_HOME=") }) {
		t.Fatalf("cargo fetch env = %v, want a private CARGO_HOME", env)
	}
	if lookPathIn("rustc", sandboxPathEnv()) != "" && !slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "RUSTC=") }) {
		t.Fatalf("cargo fetch env = %v, want RUSTC pinned", env)
	}
}

// The fetch's masks cover more than isWorkspaceSecretPath: the whole .git and
// .cargo directories, secrets inside ignored directories, and the extra
// credential shapes; makewand's own sandbox HOME is left writable.
func TestNetworkedFetchMasksExtendedSecrets_GEC1021R2(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, ".git", "config"), "[remote]\n  url = https://x:token@h/r.git\n")
	writeFile(t, filepath.Join(ws, ".cargo", "config.toml"), "[build]\nrustc = \"../evil.sh\"\n")
	writeFile(t, filepath.Join(ws, ".envrc"), "export AWS_SECRET=1")
	writeFile(t, filepath.Join(ws, "deploy", "tls.pem"), "key")
	writeFile(t, filepath.Join(ws, "deploy", "server.key"), "key")
	writeFile(t, filepath.Join(ws, ".ssh2", "id_ed25519"), "key")
	writeFile(t, filepath.Join(ws, "gcp-service-account.json"), "{}")
	writeFile(t, filepath.Join(ws, "prod.tfvars"), "secret=1")
	writeFile(t, filepath.Join(ws, "credentials.json"), "{}")
	writeFile(t, filepath.Join(ws, "node_modules", "dep", ".env"), "SECRET=1")
	writeFile(t, filepath.Join(ws, "main.go"), "package main\n")
	// makewand's own sandbox HOME must stay writable for the fetch's caches.
	writeFile(t, filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath), ".cargo", "registry", "cache", "x-1.0.0.crate"), "cache")
	writeFile(t, filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath), "id_rsa"), "cachekey")

	masks, err := sandboxWorkspaceSecretMasks(ws)
	if err != nil {
		t.Fatal(err)
	}
	masked := map[string]string{}
	for i := 0; i < len(masks); {
		switch masks[i] {
		case "--tmpfs":
			masked[masks[i+1]] = "tmpfs"
			i += 2
		case "--ro-bind":
			masked[masks[i+2]] = masks[i+1]
			i += 3
		default:
			i++
		}
	}
	wantTmpfs := []string{filepath.Join(ws, ".git"), filepath.Join(ws, ".cargo")}
	for _, dir := range wantTmpfs {
		if masked[dir] != "tmpfs" {
			t.Errorf("%s masked with %q, want a tmpfs", dir, masked[dir])
		}
	}
	wantDevNull := []string{
		filepath.Join(ws, ".envrc"),
		filepath.Join(ws, "deploy", "tls.pem"),
		filepath.Join(ws, "deploy", "server.key"),
		filepath.Join(ws, ".ssh2", "id_ed25519"),
		filepath.Join(ws, "gcp-service-account.json"),
		filepath.Join(ws, "prod.tfvars"),
		filepath.Join(ws, "credentials.json"),
		filepath.Join(ws, "node_modules", "dep", ".env"),
	}
	for _, f := range wantDevNull {
		if masked[f] != os.DevNull {
			t.Errorf("%s masked with %q, want %s", f, masked[f], os.DevNull)
		}
	}
	if _, ok := masked[filepath.Join(ws, "main.go")]; ok {
		t.Error("a non-secret source file must not be masked")
	}
	// The sandbox HOME is skipped wholesale, so its cache files are untouched.
	sandboxHome := filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath))
	for target := range masked {
		if strings.HasPrefix(target, sandboxHome+string(filepath.Separator)) {
			t.Errorf("the sandbox HOME must not be masked, got %s", target)
		}
	}
}

// Classification: a pip requirement that does not exist on the default index is
// the candidate's own broken dependency (a candidate failure), while one that
// exists only as a source distribution, or a custom index that cannot be fact
// checked, stays a conservative environment verdict.
func TestNonexistentPipRequirementIsCandidateFailure_GEC1021R2(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     pypiProjectStatus
		args       []string
		wantAbsent bool
	}{
		{"absent on default index", pypiProjectAbsent, []string{"install", "-r", "requirements.txt"}, true},
		{"present but sdist-only", pypiProjectPresent, []string{"install", "-r", "requirements.txt"}, false},
		{"unknown stays environment", pypiProjectUnknown, []string{"install", "-r", "requirements.txt"}, false},
		{"custom index is not fact checked", pypiProjectAbsent, []string{"install", "-i", "https://mirror.example/simple/", "-r", "requirements.txt"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := newNeedProject(t, "requirements.txt", "boguspkg\n")
			old := pypiProjectExists
			pypiProjectExists = func(string) pypiProjectStatus { return tc.status }
			t.Cleanup(func() { pypiProjectExists = old })
			plan := ExecPlan{Kind: "deps", Command: "pip", Args: tc.args}
			if got := project.pipProjectAbsent(plan, "boguspkg==1.0"); got != tc.wantAbsent {
				t.Fatalf("pipProjectAbsent = %v, want %v", got, tc.wantAbsent)
			}
		})
	}
}

// Classification: a loopback build/install stage whose dependencies were
// prefetched is never reclassified as an environment outage from its own
// output, even output that forges pip's "No matching distribution" box.
func TestPrefetchedBuildFailureStaysCandidate_GEC1021R2(t *testing.T) {
	project := newNeedProject(t, "requirements.txt", "six==1.16.0\n")
	plan := ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "-r", "requirements.txt"}}
	forged := &ExecResult{ExitCode: 1, Stderr: "WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NewConnectionError(...)'\n" +
		"ERROR: No matching distribution found for six"}
	// A plain offline build (not prefetched) is reclassified as environment.
	if note, err := project.dependencyRegistryError(context.Background(), verifyExecEnvironment{}, plan, forged, sandboxNetwork{kind: sandboxNetworkNone}); err == nil || note != "" {
		t.Fatalf("offline build note=%q err=%v, want the environment verdict (base behaviour)", note, err)
	}
	// The prefetched build's identical output is the candidate's own failure.
	note, err := project.dependencyRegistryError(context.Background(), verifyExecEnvironment{}, plan, forged, sandboxNetwork{kind: sandboxNetworkNone, prefetched: true})
	if err != nil {
		t.Fatalf("prefetched build err=%v, want no environment error", err)
	}
	if !strings.Contains(note, "prefetched") {
		t.Fatalf("prefetched build note=%q, want it to say the dependencies were prefetched", note)
	}
}

// With outbound network available, a dependency npm fetches by running code
// (a local-directory or git dependency) makes the networked fetch refuse to
// run at all - npm is never invoked - and the failure is an environment
// condition, not a candidate failure. A registry-only tree still runs.
func TestNpmNonRegistryFetchRefused_GEC1021R2(t *testing.T) {
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	t.Run("local dependency is refused and npm never runs", func(t *testing.T) {
		env := installFakeUserNet(t, fakeUserNetOptions{}) // slirp4netns present
		project, err := NewProject("npm-nonreg", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(project.Path, "package.json"), `{"name":"x","dependencies":{"dep":"file:./dep"}}`)
		writeFile(t, filepath.Join(project.Path, "dep", "package.json"), `{"name":"dep","version":"1.0.0","scripts":{"prepare":"touch PREPARED"}}`)
		_, err = project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}})
		if !errorIsDepNetworkUnavailable(err) {
			t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable (the fetch runs project code)", err)
		}
		if data := readLog(t, env.log); strings.Contains(data, "ran") {
			t.Fatalf("npm must not run when the fetch is refused; bwrap log:\n%s", data)
		}
	})
	t.Run("registry-only tree runs", func(t *testing.T) {
		env := installFakeUserNet(t, fakeUserNetOptions{})
		stubNpmVersion(t, 10)
		project, err := NewProject("npm-reg", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(project.Path, "package.json"), `{"name":"x","dependencies":{"left-pad":"^1.3.0"}}`)
		// Since round 5 a networked npm fetch installs only from a lockfile
		// makewand has inspected, and before npm 12 that must be the project's
		// own (TestNpmFetchResolvesThroughInspectedLockfile_GEC1021R5).
		writeFile(t, filepath.Join(project.Path, "package-lock.json"), registryLeftPadLock)
		if _, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install"}}); errorIsDepNetworkUnavailable(err) {
			t.Fatalf("a registry-only tree must not be refused: %v", err)
		}
		if data := readLog(t, env.log); !strings.Contains(data, "ran") {
			t.Fatalf("the npm fetch should have run; bwrap log:\n%s", data)
		}
	})
}

// Live: in the real sandbox, a build/install stage that fails with forged pip
// output (a malicious backend printing pip's "No matching distribution" box)
// stays a candidate failure - its dependencies were prefetched, so it reached
// no registry - and is never reclassified as an environment outage.
func TestForgedBuildOutputStaysCandidateFailure_GEC1021R2(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	project := sandboxDepsProject(t)
	tools := filepath.Join(project.Path, "tools")
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	// A fake pip that downloads fine but whose offline install forges pip's own
	// connection-failure + no-distribution lines, as a malicious build backend
	// would. Only the install (project code) forges them; the download does not.
	const pip = `case "$1" in
download) exit 0 ;;
esac
echo "WARNING: Retrying (Retry(total=0, connect=None, read=None, redirect=None, status=None)) after connection broken by 'NewConnectionError(...)'" >&2
echo "ERROR: No matching distribution found for six" >&2
exit 1
`
	writeExecScript(t, filepath.Join(tools, "pip"), pip)
	if err := project.WriteFiles([]ExtractedFile{{Path: "requirements.txt", Content: "six==1.16.0\n"}}); err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}})
	if errorIsDepNetworkUnavailable(err) {
		t.Fatalf("forged build output must not be reclassified as an environment outage: %v", err)
	}
	if err != nil {
		t.Fatalf("RunVerificationPlan: %v", err)
	}
	if result == nil || result.ExitCode == 0 {
		t.Fatalf("result = %+v, want the failing build reported as the candidate's own failure", result)
	}
}

// sandboxDepsProject opens a project for a live verification-sandbox test with
// a hermetic HOME and no proxy or acknowledged host-exec opt-in.
func sandboxDepsProject(t *testing.T) *Project {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := disableGoTelemetry(home); err != nil {
		t.Fatal(err)
	}
	unsetProxyEnv(t)
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_SANDBOX_MEMORY_MAX", "2G")
	t.Setenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE", "")
	project, err := NewProject("deps-sandbox", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func errorIsDepNetworkUnavailable(err error) bool {
	return errors.Is(err, ErrDependencyNetworkUnavailable)
}

// fetchEnvOf renders a fetch plan's env for a failure message (nil-safe).
func fetchEnvOf(plan *ExecPlan) []string {
	if plan == nil {
		return nil
	}
	return plan.env
}
