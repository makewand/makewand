package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Fetch-only dependency networking (GEC-1021-1): what decides, before the
// network is up, what a split dependency plan's networked fetch may run.

// rustupChannelRe matches a toolchain a workspace rust-toolchain file may name
// for the networked cargo fetch: stable, beta, nightly or a version (with an
// optional beta suffix), an optional archive date and an optional (partial)
// host triple. It admits no '/' or '\', so the value always names a toolchain
// and never a path (rustup treats a RUSTUP_TOOLCHAIN containing either as a
// path toolchain).
var rustupChannelRe = regexp.MustCompile(`^(?:stable|beta|nightly|[0-9]+\.[0-9]+(?:\.[0-9]+)?(?:-beta(?:\.[0-9]+)?)?)(?:-[0-9]{4}-[0-9]{2}-[0-9]{2})?(?:-[a-z0-9_]+){0,4}$`)

// rustupToolchainNameRe matches a toolchain the host's own rustup settings may
// hold (the default, a directory override): an official toolchain or a custom
// one linked with `rustup toolchain link`, never a path.
var rustupToolchainNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// rustToolchainFile is what the workspace's rust-toolchain file asks rustup
// for.
type rustToolchainFile struct {
	name    string // the file name, rust-toolchain or rust-toolchain.toml
	channel string // the channel it names, as written ("" when none)
	path    bool   // it names a toolchain by path
}

// workspaceRustToolchain returns the toolchain file rustup honours in the
// workspace root: rust-toolchain before rust-toolchain.toml, as rustup picks
// the former (with a warning) when both exist. A file that exists but cannot
// be read reports neither a channel nor a path.
func (p *Project) workspaceRustToolchain() (rustToolchainFile, bool) {
	for _, name := range []string{"rust-toolchain", "rust-toolchain.toml"} {
		full, err := p.validatePath(name, false)
		if err != nil {
			continue
		}
		if info, err := os.Stat(full); errors.Is(err, fs.ErrNotExist) || (err == nil && info.IsDir()) {
			continue // rustup reads neither a missing file nor a directory
		}
		content, err := p.ReadFile(name)
		if err != nil {
			return rustToolchainFile{name: name}, true
		}
		return parseRustToolchainFile(name, content), true
	}
	return rustToolchainFile{}, false
}

// parseRustToolchainFile reads a rust-toolchain file the way rustup 1.28 does:
// a legacy rust-toolchain of a single line names a channel, or a path when the
// line is absolute; anything else is TOML, whose [toolchain] table (also as
// dotted keys or an inline table) holds channel or path.
func parseRustToolchainFile(name, content string) rustToolchainFile {
	file := rustToolchainFile{name: name}
	if name == "rust-toolchain" && rustLineCount(content) == 1 {
		line := strings.TrimSpace(content)
		if filepath.IsAbs(line) {
			file.path = true
		} else {
			file.channel = line
		}
		return file
	}
	for _, entry := range parseTOML(content).entries {
		key := entry.key
		if table := tomlKey(entry.table); table != "" {
			key = table + "." + key
		}
		switch key {
		case "toolchain.path":
			file.path = true
		case "toolchain.channel":
			file.channel = firstTOMLString(entry.value)
		case "toolchain":
			fields := tomlInlineTable(entry.value)
			if _, ok := fields["path"]; ok {
				file.path = true
			}
			if value, ok := fields["channel"]; ok {
				file.channel = firstTOMLString(value)
			}
		}
	}
	return file
}

// rustLineCount counts lines the way Rust's str::lines does: a final newline
// ends the last line rather than starting another.
func rustLineCount(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// rustupSettings is what makewand reads of the host rustup's settings.toml.
type rustupSettings struct {
	defaultToolchain string
	// overrides maps a directory to the toolchain `rustup override set` chose
	// for it.
	overrides map[string]string
}

func readRustupSettings(rustupHome string) rustupSettings {
	settings := rustupSettings{overrides: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(rustupHome, "settings.toml")) //nolint:gosec // G304: the host rustup's own settings
	if err != nil {
		return settings
	}
	for _, entry := range parseTOML(string(data)).entries {
		switch {
		case entry.table == "" && entry.key == "default_toolchain":
			settings.defaultToolchain = firstTOMLString(entry.value)
		case tomlKey(entry.table) == "overrides":
			settings.overrides[filepath.Clean(entry.key)] = firstTOMLString(entry.value)
		}
	}
	return settings
}

// cargoFetchToolchain returns the rustup toolchain the networked cargo fetch is
// pinned to with RUSTUP_TOOLCHAIN, or why that fetch cannot run with the
// network. Unpinned, a rustup proxy takes the toolchain from the workspace's
// rust-toolchain file, and one naming a toolchain by path makes the fetch run
// that path's cargo, project code, with the network (confirmed on rustup
// 1.28; RUSTC does not help, the cargo proxy itself is resolved first). With
// RUSTUP_TOOLCHAIN set rustup never reads the file.
//
// The pin is the toolchain rustup itself would pick, read from the workspace
// and the host's rustup settings and never a path: a directory override of the
// workspace, the workspace's toolchain file (cargo fetch runs in the workspace
// root, the only workspace directory rustup searches), a directory override of
// a directory above it, the default toolchain. A toolchain file naming a path,
// a channel rustupChannelRe rejects, or a toolchain the pin cannot replace
// because the host has no default toolchain refuses the networked fetch.
// Without a rustup home cargo and rustc are no rustup proxies and RUSTC is
// pinned (cargoFetchEnv): there is nothing to pin or refuse.
func (p *Project) cargoFetchToolchain(projectDir string) (pin, refusal string) {
	home, err := verifyUserHome()
	if err != nil {
		home = ""
	}
	rustupHome := hostRustupHome(home, verifyGetenv)
	if info, err := os.Stat(rustupHome); err != nil || !info.IsDir() {
		return "", ""
	}
	settings := readRustupSettings(rustupHome)
	override := func(dir string) string {
		if name := settings.overrides[dir]; rustupToolchainNameRe.MatchString(name) {
			return name
		}
		return ""
	}
	projectDir = filepath.Clean(projectDir)
	if name := override(projectDir); name != "" {
		return name, ""
	}
	file, found := p.workspaceRustToolchain()
	switch {
	case found && file.path:
		return "", "the project's " + file.name + " selects a rustup toolchain by path, whose cargo and rustc the networked cargo fetch would run as project code; makewand pins that fetch to a named rustup toolchain and only lets a dependency download use the network, so name a channel there (stable, beta, nightly or a version) or install the project's dependencies outside makewand"
	case found && file.channel != "" && !rustupChannelRe.MatchString(file.channel):
		return "", fmt.Sprintf("the project's %s names the rustup toolchain %q, which is not a channel makewand can pin the networked cargo fetch to (stable, beta, nightly or a version, with an optional date and host triple); makewand only lets a dependency download use the network, so name such a channel there or install the project's dependencies outside makewand", file.name, truncateForNote(file.channel))
	case found && file.channel != "":
		return file.channel, ""
	}
	for dir := filepath.Dir(projectDir); ; dir = filepath.Dir(dir) {
		if name := override(dir); name != "" {
			return name, ""
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	if rustupToolchainNameRe.MatchString(settings.defaultToolchain) {
		return settings.defaultToolchain, ""
	}
	if found {
		return "", "the project has a " + file.name + ", but this host's rustup has no default toolchain makewand could pin the networked cargo fetch to in its place; set one with `rustup default stable` or install the project's dependencies outside makewand"
	}
	return "", ""
}

// truncateForNote shortens project-controlled text quoted in a message.
func truncateForNote(s string) string {
	const limit = 80
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// npmNetworkedLockMajor is the first npm major version whose lockfile-only
// resolution makewand runs with the network: npm 12 refuses git and URL
// dependencies by default (makewand forces allow-git=none), and honours
// --ignore-scripts when it prepares a git or directory dependency (verified
// on npm 12.2.0, pacote 22). Earlier versions (npm 10.8.2, pacote 18) run
// the prepare script of a hosted git dependency pinned to a commit while
// merely resolving it, --package-lock-only --ignore-scripts or not, so
// makewand resolves with them offline, from the project's own lockfile.
const npmNetworkedLockMajor = 12

// maxNpmManifestBytes bounds the package.json and lockfiles makewand copies
// and inspects for an npm fetch.
const maxNpmManifestBytes = 64 << 20

// npmProjectFiles are what npm resolves a project's dependencies from: the
// manifest and the lockfiles it follows (npm-shrinkwrap.json before
// package-lock.json, and yarn.lock, whose resolutions npm 7+ follows when it
// has no package lock).
var npmProjectFiles = []string{"package.json", "npm-shrinkwrap.json", "package-lock.json", "yarn.lock"}

var (
	// npmAllowCodeRe matches npm 12's error code for a dependency its
	// allow-git/allow-remote/allow-file/allow-directory setting refuses.
	npmAllowCodeRe = regexp.MustCompile(`^npm (?:ERR!|error) code EALLOW(GIT|REMOTE|FILE|DIRECTORY)$`)
	// npmRefusedFetchRe captures the spec npm refused to fetch.
	npmRefusedFetchRe = regexp.MustCompile(`^npm (?:ERR!|error) Refusing to fetch "(.+)"$`)
	// npmNotCachedRe matches npm's error code for a request --offline forbids.
	npmNotCachedRe = regexp.MustCompile(`^npm (?:ERR!|error) code ENOTCACHED$`)
)

// npmFetchUnavailable returns why an npm dependency plan cannot fetch with the
// network, read from the project's files before anything runs, or "": a
// subcommand other than install or ci, or a project npm would take some
// dependency for from anywhere but the registry (npmNonRegistryDependency).
func (p *Project) npmFetchUnavailable(plan ExecPlan) string {
	if len(plan.Args) == 0 || (plan.Args[0] != "install" && plan.Args[0] != "i" && plan.Args[0] != "ci") {
		return "makewand runs only `npm install` or `npm ci` as a networked dependency fetch; install the project's dependencies outside makewand"
	}
	if reason := p.npmNonRegistryDependency(); reason != "" {
		return reason + "; " + npmRegistryOnlyRemedy
	}
	return ""
}

// npmRegistryOnlyRemedy ends every refusal of a networked npm fetch.
const npmRegistryOnlyRemedy = "makewand only lets a download from the npm registry use the network, so depend on registry releases or install the project's dependencies outside makewand"

// npmNonRegistryDependency returns why npm would take some of the project's
// dependencies from anywhere but the registry, or "". Before npm 12 npm runs
// the prepare script of a git, local-directory or workspace dependency even
// under --ignore-scripts (confirmed on npm 10.8.2 for each), and a URL, local
// tarball or link is not a registry download either. It reads package.json -
// a workspaces root, every dependency and override spec - and the lockfiles
// npm follows (npmLockfileNonRegistry, yarnLockNonRegistry). A file it cannot
// read is left to the lock resolution, whose own lockfile execNpmFetch
// inspects before anything is downloaded.
func (p *Project) npmNonRegistryDependency() string {
	if data, err := p.readProjectFileLimit("package.json", maxNpmManifestBytes); err == nil {
		var manifest map[string]json.RawMessage
		if json.Unmarshal(data, &manifest) == nil {
			if npmDeclaresWorkspaces(manifest["workspaces"]) {
				return "the project is an npm workspaces root (package.json workspaces), and npm before 12 runs each workspace's prepare script even under --ignore-scripts"
			}
			hasWorkspaces := p.npmHasWorkspaceConfig()
			for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
				var specs map[string]string
				if json.Unmarshal(manifest[field], &specs) != nil {
					continue
				}
				for _, name := range slices.Sorted(maps.Keys(specs)) {
					if npmNonRegistrySpec(specs[name], hasWorkspaces) {
						return "the project depends on " + name + " (" + field + ": " + truncateForNote(specs[name]) + "), which npm would fetch from somewhere other than the registry (a git repository, URL, local path or link, whose prepare script npm before 12 runs even under --ignore-scripts when it is a git repository or a directory)"
					}
				}
			}
			if spec := npmNonRegistryOverride(manifest["overrides"]); spec != "" {
				return "package.json overrides a dependency with " + truncateForNote(spec) + ", which npm would fetch from somewhere other than the registry"
			}
		}
	}
	for _, name := range []string{"npm-shrinkwrap.json", "package-lock.json"} {
		if data, err := p.readProjectFileLimit(name, maxNpmManifestBytes); err == nil {
			if dep, _ := npmLockfileNonRegistry(data); dep != "" {
				return "the project's " + name + " takes " + dep + " instead of the registry"
			}
		}
	}
	if data, err := p.readProjectFileLimit("yarn.lock", maxNpmManifestBytes); err == nil {
		if dep := yarnLockNonRegistry(string(data)); dep != "" {
			return "the project's yarn.lock, whose resolutions npm follows without a package lock, takes " + dep + " instead of the registry"
		}
	}
	return ""
}

// npmDeclaresWorkspaces reports whether a package.json workspaces value
// declares any workspace: a non-empty array, or an object with packages.
func npmDeclaresWorkspaces(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch v := value.(type) {
	case []any:
		return len(v) > 0
	case map[string]any:
		packages, _ := v["packages"].([]any)
		return len(packages) > 0
	}
	return false
}

// npmHasWorkspaceConfig reports whether a pnpm workspaces setup is declared
// (pnpm-workspace.yaml), where a workspace: dependency links in place. npm
// does not support the protocol and stops at it without fetching anything.
func (p *Project) npmHasWorkspaceConfig() bool {
	_, err := p.ReadFile("pnpm-workspace.yaml")
	return err == nil
}

// npmNonRegistrySpec reports whether npm would fetch a dependency spec from
// anywhere but the registry. It follows npm-package-arg: a registry spec is a
// version, range or dist-tag ("~1.2.3" included), or an npm: alias of one. A
// spec with a ':' (any protocol, a hosted-git shortcut such as github: or
// sourcehut:, an scp-style git URL), a '/' or '\' (a path, the user/repo
// GitHub shorthand), a leading '.' or "~/", or a tarball file name is not;
// https://github.com/user/repo is a git dependency to npm, not a tarball. A
// workspace: link is accepted where a workspaces setup declares it (npm
// rejects the protocol without fetching anything).
func npmNonRegistrySpec(spec string, hasWorkspaces bool) bool {
	spec = strings.TrimSpace(spec)
	lower := strings.ToLower(spec)
	switch {
	case strings.HasPrefix(lower, "npm:"):
		name, target := npmAliasTarget(spec[len("npm:"):])
		return !npmPackageNameRe.MatchString(name) || strings.HasPrefix(strings.ToLower(target), "npm:") || npmNonRegistrySpec(target, false)
	case strings.HasPrefix(lower, "workspace:"):
		return !hasWorkspaces
	case strings.ContainsAny(spec, ":/\\"), strings.HasPrefix(spec, "."), strings.HasPrefix(spec, "~/"):
		return true
	case strings.HasSuffix(lower, ".tgz"), strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tar"):
		return true
	}
	return false
}

// npmAliasTarget splits "name@spec" the way npm-package-arg does: the name
// ends at the first '@' after the first character (a scope's own '@' comes
// first), and a missing spec is "*".
func npmAliasTarget(target string) (name, spec string) {
	if len(target) > 1 {
		if at := strings.IndexByte(target[1:], '@'); at >= 0 {
			name, spec = target[:at+1], target[at+2:]
			if spec == "" {
				spec = "*"
			}
			return name, spec
		}
	}
	return target, "*"
}

// npmNonRegistryOverride returns the first override in package.json's
// overrides, nested ones included, whose spec is neither a registry spec nor
// a $reference to a dependency, as "package: spec", or "".
func npmNonRegistryOverride(raw json.RawMessage) string {
	var overrides map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &overrides) != nil {
		return ""
	}
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		var spec string
		if json.Unmarshal(overrides[key], &spec) == nil {
			if !strings.HasPrefix(spec, "$") && npmNonRegistrySpec(spec, false) {
				return key + ": " + spec
			}
			continue
		}
		if nested := npmNonRegistryOverride(overrides[key]); nested != "" {
			return nested
		}
	}
	return ""
}

// npmLockfileNonRegistry returns the first package an npm lockfile
// (package-lock.json or npm-shrinkwrap.json, any lockfileVersion) does not
// take from the registry, as "name from source", or "": a link, a local
// package (an entry outside node_modules: a workspace or a directory
// dependency), or one resolved from a git repository, a file:, a path or a
// URL that is not a registry tarball (npmRegistryTarball). Every entry of the
// packages map (v2/v3) and of the nested dependencies tree (v1) is read
// structurally; an entry it cannot read is an error.
func npmLockfileNonRegistry(data []byte) (string, error) {
	var lock struct {
		Packages     map[string]json.RawMessage `json:"packages"`
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return "", err
	}
	for _, path := range slices.Sorted(maps.Keys(lock.Packages)) {
		if path == "" {
			continue // the project itself
		}
		var entry struct {
			Name     string `json:"name"`
			Version  string `json:"version"`
			Resolved string `json:"resolved"`
			Link     bool   `json:"link"`
		}
		if err := json.Unmarshal(lock.Packages[path], &entry); err != nil {
			return "", fmt.Errorf("package %s: %w", path, err)
		}
		name := npmLockPackageName(path)
		if entry.Name != "" {
			name = entry.Name
		}
		switch {
		case entry.Link:
			return name + " as a link to " + truncateForNote(entry.Resolved), nil
		case !strings.HasPrefix(path, "node_modules/") && !strings.Contains(path, "/node_modules/"):
			return "the local package " + truncateForNote(path), nil
		}
		if source := npmLockSource(name, entry.Version, entry.Resolved); source != "" {
			return name + " from " + source, nil
		}
	}
	return npmLockV1NonRegistry(lock.Dependencies, 0)
}

// npmLockV1NonRegistry walks a lockfile v1 dependencies tree (see
// npmLockfileNonRegistry).
func npmLockV1NonRegistry(deps map[string]json.RawMessage, depth int) (string, error) {
	if depth > 64 {
		return "", errors.New("dependencies nested too deeply")
	}
	for _, name := range slices.Sorted(maps.Keys(deps)) {
		var entry struct {
			Version      string                     `json:"version"`
			Resolved     string                     `json:"resolved"`
			Dependencies map[string]json.RawMessage `json:"dependencies"`
		}
		if err := json.Unmarshal(deps[name], &entry); err != nil {
			return "", fmt.Errorf("dependency %s: %w", name, err)
		}
		if source := npmLockSource(name, entry.Version, entry.Resolved); source != "" {
			return name + " from " + source, nil
		}
		if dep, err := npmLockV1NonRegistry(entry.Dependencies, depth+1); dep != "" || err != nil {
			return dep, err
		}
	}
	return "", nil
}

// npmLockPackageName returns the package name from a lockfile v2/v3 package
// path such as "node_modules/@scope/pkg" or "node_modules/a/node_modules/b".
func npmLockPackageName(path string) string {
	if i := strings.LastIndex(path, "node_modules/"); i >= 0 {
		return path[i+len("node_modules/"):]
	}
	return path
}

// npmLockSource returns where a lockfile entry for name@version resolved as
// resolved comes from, when that is not the registry, or "". Lockfile v1
// records a git, file: or URL dependency's spec as its version; an entry
// without resolved is fetched as name@version from the registry.
func npmLockSource(name, version, resolved string) string {
	if strings.ContainsAny(version, ":/\\") {
		alias, ok := strings.CutPrefix(strings.ToLower(version), "npm:")
		if !ok {
			return truncateForNote(version)
		}
		name, version = npmAliasTarget(version[len(version)-len(alias):])
	}
	resolved = strings.TrimSpace(resolved)
	lower := strings.ToLower(resolved)
	switch {
	case resolved == "":
		return ""
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		if npmRegistryTarball(resolved, name, version) {
			return ""
		}
		return "the URL " + truncateForNote(resolved)
	}
	return truncateForNote(resolved) // git+, git:, ssh:, file:, a path
}

// npmRegistryTarball reports whether a resolved URL is the registry tarball of
// name@version: an http(s) URL whose path ends in /<name>/-/<base>-<version>.tgz,
// base being the name without its scope - the layout of the npm registry and
// of the mirrors and private registries that serve its API.
func npmRegistryTarball(raw, name, version string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || name == "" {
		return false
	}
	base := name[strings.LastIndexByte(name, '/')+1:]
	marker := "/" + name + "/-/" + base + "-"
	i := strings.LastIndex(u.Path, marker)
	if i < 0 {
		return false
	}
	file := u.Path[i+len(marker):]
	if version != "" {
		return file == version+".tgz"
	}
	return strings.HasSuffix(file, ".tgz") && !strings.Contains(file, "/")
}

// yarnLockNonRegistry returns the first package a yarn.lock in yarn's v1
// format - the one npm follows - takes from anywhere but a registry, as
// "name (spec)" or "name from source", or "": an entry whose spec is not a
// registry spec, or whose resolved URL is not a registry tarball. npm ignores
// a berry (yarn 2+) lockfile (confirmed on npm 10.8.2 and 12.2.0).
func yarnLockNonRegistry(content string) string {
	type entry struct {
		specs             []string
		version, resolved string
	}
	var entries []entry
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "__metadata:":
			return "" // a berry lockfile
		case line == "", strings.HasPrefix(line, "#"):
		case !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":"):
			var specs []string
			for _, spec := range strings.Split(strings.TrimSuffix(line, ":"), ",") {
				specs = append(specs, strings.Trim(strings.TrimSpace(spec), `"`))
			}
			entries = append(entries, entry{specs: specs})
		case len(entries) > 0 && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			e := &entries[len(entries)-1]
			if value, ok := strings.CutPrefix(line, "  version "); ok {
				e.version = strings.Trim(value, `"`)
			} else if value, ok := strings.CutPrefix(line, "  resolved "); ok {
				e.resolved = strings.Trim(value, `"`)
			}
		}
	}
	for _, e := range entries {
		for _, spec := range e.specs {
			if name, target := npmAliasTarget(spec); npmNonRegistrySpec(target, false) {
				return name + " (" + truncateForNote(spec) + ")"
			}
		}
		if len(e.specs) == 0 {
			continue
		}
		name, target := npmAliasTarget(e.specs[0])
		if alias, ok := strings.CutPrefix(target, "npm:"); ok {
			name, _ = npmAliasTarget(alias) // an alias resolves the aliased package
		}
		if source := npmLockSource(name, e.version, e.resolved); source != "" {
			return name + " from " + source
		}
	}
	return ""
}

// readProjectFileLimit reads a regular file of the project like ReadFile,
// within its own size limit. A missing file is an error wrapping
// fs.ErrNotExist.
func (p *Project) readProjectFileLimit(relPath string, limit int64) ([]byte, error) {
	if _, err := p.validatePath(relPath, false); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.Clean(relPath))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", relPath)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", relPath, limit)
	}
	return data, nil
}

// npmVersionProbe is npmMajorVersion; a package variable so tests choose the
// npm version without installing one.
var npmVersionProbe = npmMajorVersion

// npmMajorVersion returns the major version of the npm the sandbox runs - the
// first npm on the sandbox PATH, whose install (the npm package that bin/npm
// links into, as npm installs itself) records it in its package.json - or 0
// when that cannot be told. It runs nothing.
func npmMajorVersion() int {
	bin := lookPathIn("npm", sandboxPathEnv())
	if bin == "" {
		return 0
	}
	cli, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(cli)), "package.json")) //nolint:gosec // G304: the npm install the sandbox PATH resolves
	if err != nil {
		return 0
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &pkg) != nil || pkg.Name != "npm" {
		return 0
	}
	major, err := strconv.Atoi(strings.SplitN(pkg.Version, ".", 2)[0])
	if err != nil {
		return 0
	}
	return major
}

// execNpmFetch runs an npm dependency fetch that has outbound network so that
// npm downloads exactly the tree of a lockfile makewand has inspected. The
// project's manifest and lockfiles are copied into a scratch project in the
// sandbox HOME, where `npm install --package-lock-only --ignore-scripts`
// resolves the full tree, transitive dependencies included, without
// installing or running anything (confirmed on npm 10.8.2 and 12.2.0 with
// direct and transitive git+file:// dependencies, a file: directory, a
// workspaces root and a yarn.lock resolving to git). npm 12 or later resolves
// with the network; earlier versions resolve offline, from the project's own
// lockfile, which must hold every dependency, since they run the prepare
// script of a hosted git dependency pinned to a commit while resolving it.
// The lockfile the resolution writes is inspected (npmLockfileNonRegistry)
// and, when it takes every package from the registry, put in place for the
// real fetch. A fetch that would not have left a lockfile behind
// (--no-package-lock, or a failed install) restores the project's own.
func (p *Project) execNpmFetch(ctx context.Context, env verifyExecEnvironment, projectDir string, fetch ExecPlan, readOnly []string) (*ExecResult, sandboxNetwork, error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	refuse := func(result *ExecResult, network sandboxNetwork, reason string) (*ExecResult, sandboxNetwork, error) {
		if result == nil {
			result = &ExecResult{ExitCode: 1}
		}
		result.Note = reason
		return result, network, fmt.Errorf("%w (%s)", ErrDependencyNetworkUnavailable, reason)
	}
	const oldNpm = "the npm in use (before 12) can run a git dependency's prepare script while it resolves dependencies with the network, even under --ignore-scripts; "

	if _, err := p.readProjectFileLimit("package.json", maxNpmManifestBytes); errors.Is(err, fs.ErrNotExist) {
		// No manifest: npm has nothing of the project's to resolve, and the
		// lockfiles npmFetchUnavailable inspected are all it could install.
		return p.execVerificationStep(ctx, env, projectDir, fetch, readOnly, true)
	}
	scratch, err := os.MkdirTemp(filepath.Join(projectDir, filepath.FromSlash(sandboxHomeRelPath)), "makewand-npm-lock-")
	if err != nil {
		return nil, none, fmt.Errorf("create the npm lock resolution directory: %w", err)
	}
	defer os.RemoveAll(scratch)
	lockName := ""
	for _, name := range npmProjectFiles {
		data, err := p.readProjectFileLimit(name, maxNpmManifestBytes)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return refuse(nil, none, "makewand cannot read the project's "+name+" to inspect it before the networked npm fetch ("+err.Error()+"); "+npmRegistryOnlyRemedy)
		}
		if err := os.WriteFile(filepath.Join(scratch, name), data, 0o600); err != nil {
			return nil, none, fmt.Errorf("copy %s for the npm lock resolution: %w", name, err)
		}
		if lockName == "" && (name == "npm-shrinkwrap.json" || name == "package-lock.json") {
			lockName = name
		}
	}
	networked := npmVersionProbe() >= npmNetworkedLockMajor
	if !networked && lockName == "" {
		return refuse(nil, none, "the project has no package-lock.json, and "+oldNpm+"commit a lockfile (npm install --package-lock-only) or use npm 12 or later; "+npmRegistryOnlyRemedy)
	}

	resolve := fetch
	resolve.Args = []string{"install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund", "--prefix", scratch}
	if !networked {
		resolve.Args = append(resolve.Args, "--offline")
	}
	result, network, err := p.execVerificationStep(ctx, env, projectDir, resolve, readOnly, networked)
	if err != nil {
		return result, network, err
	}
	if result != nil && result.ExitCode != 0 {
		// The resolution runs no project code, so npm's report is its own.
		if reason := npmAllowRefusal(result); reason != "" {
			return refuse(result, network, reason+"; "+npmRegistryOnlyRemedy)
		}
		if !networked && npmNotCached(result) {
			return refuse(result, network, "the project's "+lockName+" does not hold every dependency, so npm would resolve the rest with the network, and "+oldNpm+"update the lockfile (npm install --package-lock-only) or use npm 12 or later; "+npmRegistryOnlyRemedy)
		}
		return result, network, nil // a failed fetch: the outage check or the candidate
	}
	if lockName == "" {
		lockName = "package-lock.json"
	}
	lock, err := readNpmLockfile(filepath.Join(scratch, lockName))
	if err != nil {
		return refuse(result, network, "npm resolved the dependencies but left no "+lockName+" makewand could inspect ("+err.Error()+"); "+npmRegistryOnlyRemedy)
	}
	dep, err := npmLockfileNonRegistry(lock)
	if err != nil {
		return refuse(result, network, "makewand cannot inspect the "+lockName+" npm resolved ("+err.Error()+"); "+npmRegistryOnlyRemedy)
	}
	if dep != "" {
		return refuse(result, network, "the project's dependencies take "+dep+" instead of the registry, which npm before 12 prepares by running its scripts even under --ignore-scripts when it is a git repository or a directory; "+npmRegistryOnlyRemedy)
	}
	restore, err := p.placeNpmLockfile(lockName, lock)
	if err != nil {
		return nil, none, fmt.Errorf("put the inspected %s in place for the npm fetch: %w", lockName, err)
	}
	install := fetch
	install.Args = slices.DeleteFunc(slices.Clone(fetch.Args), func(arg string) bool { return arg == "--no-package-lock" })
	result, network, err = p.execVerificationStep(ctx, env, projectDir, install, readOnly, true)
	if err != nil || result == nil || result.ExitCode != 0 || slices.Contains(fetch.Args, "--no-package-lock") {
		restore()
	}
	if err == nil && result != nil && result.ExitCode != 0 {
		if reason := npmAllowRefusal(result); reason != "" {
			return refuse(result, network, reason+"; "+npmRegistryOnlyRemedy)
		}
	}
	return result, network, err
}

// readNpmLockfile reads the lockfile the lock resolution wrote.
func readNpmLockfile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxNpmManifestBytes {
		return nil, fmt.Errorf("%s is not a regular file of at most %d bytes", filepath.Base(path), maxNpmManifestBytes)
	}
	return os.ReadFile(path) //nolint:gosec // G304: in the lock resolution's own scratch directory
}

// placeNpmLockfile writes the inspected lockfile into the project for the
// real fetch, unless the project's own one already says the same, and
// returns what restores the project's previous lockfile.
func (p *Project) placeNpmLockfile(name string, lock []byte) (func(), error) {
	before, err := p.readProjectFileLimit(name, maxNpmManifestBytes)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if existed && npmLockfilesEqual(before, lock) {
		return func() {}, nil
	}
	mode := fs.FileMode(0o644)
	if err := p.writeFileMode(name, string(lock), &mode); err != nil {
		return nil, err
	}
	return func() {
		if existed {
			_ = p.writeFileMode(name, string(before), nil)
			return
		}
		if root, err := os.OpenRoot(p.Path); err == nil {
			_ = root.Remove(name)
			_ = root.Close()
		}
	}, nil
}

// npmLockfilesEqual reports whether two lockfiles hold the same JSON.
func npmLockfilesEqual(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// npmAllowRefusal returns, from npm's own report, why npm refused a dependency
// under its allow-git, allow-remote, allow-file or allow-directory setting
// (npm 12 refuses git and URL dependencies by default, and makewand sets
// allow-git=none), or "".
func npmAllowRefusal(result *ExecResult) string {
	kind, spec := "", ""
	for _, line := range outputLines(result) {
		if m := npmAllowCodeRe.FindStringSubmatch(line); m != nil && kind == "" {
			kind = strings.ToLower(m[1])
		}
		if m := npmRefusedFetchRe.FindStringSubmatch(line); m != nil && spec == "" {
			spec = m[1]
		}
	}
	switch {
	case kind == "":
		return ""
	case spec == "":
		spec = "a dependency"
	}
	return "npm refused to fetch " + truncateForNote(spec) + ", a " + kind + " dependency, which makewand never lets npm download with the network"
}

// npmNotCached reports whether npm stopped at a request --offline forbade.
func npmNotCached(result *ExecResult) bool {
	return slices.ContainsFunc(outputLines(result), npmNotCachedRe.MatchString)
}
