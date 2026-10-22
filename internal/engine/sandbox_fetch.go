package engine

import (
	"archive/zip"
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
// network, read from its command line and the project's files before anything
// runs, or "": a subcommand other than install or ci; any package argument (a
// local path, a tarball, a git spec or a registry name, which npm would
// install without the lockfile makewand inspects) or an option that is not
// one npmNetworkedOption accepts; or a project npm would take some dependency
// for from anywhere but the registry (npmNonRegistryDependency).
func (p *Project) npmFetchUnavailable(plan ExecPlan) string {
	if len(plan.Args) == 0 || !slices.Contains([]string{"install", "i", "ci", "clean-install"}, plan.Args[0]) {
		return "makewand runs only `npm install` or `npm ci` as a networked dependency fetch; install the project's dependencies outside makewand"
	}
	for _, arg := range plan.Args[1:] {
		if !npmNetworkedOption(arg) {
			return "makewand runs a networked npm fetch only as `npm ci` or `npm install` without package arguments, from a lockfile it has inspected, and the command line holds " + truncateForNote(arg) + " (a package argument - a local path, tarball or git spec - would be installed without that lockfile); install the project's dependencies outside makewand"
		}
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
			// a blank line or a comment
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

// npmMajorVersion returns the major version of the npm a networked fetch runs
// (npm, resolved by fetchToolPath) - its install, the npm package that bin/npm
// links into as npm installs itself, records it in its package.json - or 0
// when that cannot be told. It runs nothing.
func npmMajorVersion(npm string) int {
	if !filepath.IsAbs(npm) {
		return 0
	}
	cli, err := filepath.EvalSymlinks(npm)
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(cli)), "package.json")) //nolint:gosec // G304: the trusted npm install a fetch runs
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

// execNpmFetch runs an npm dependency plan that has outbound network so that
// npm downloads exactly the tree of a lockfile makewand has inspected, in a
// manifest-only fetch directory that sees no part of the workspace: the
// project's package.json and its lockfile (npm-shrinkwrap.json before
// package-lock.json) are copied in, `npm install --package-lock-only
// --ignore-scripts` resolves the full tree into the lockfile there without
// installing or running anything (confirmed on npm 10.8.2 and 12.2.0 with
// direct and transitive git+file:// dependencies, a file: directory, a
// workspaces root and a yarn.lock resolving to git), the lockfile is inspected
// (npmLockfileNonRegistry), and `npm ci --ignore-scripts` installs exactly it,
// with the shared npm cache. npm 12 or later resolves with the network;
// earlier versions resolve offline, from the project's own lockfile, which
// must hold every dependency, since they run the prepare script of a hosted
// git dependency pinned to a commit while resolving it. `npm ci` resolves
// nothing, so its lockfile is only inspected. The host then puts the
// node_modules the install produced into the workspace, and, for an install
// that writes a lockfile, the resolved lockfile (placeFetchTree,
// writeBackWorkspaceFile).
func (p *Project) execNpmFetch(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string) (*ExecResult, sandboxNetwork, error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	const oldNpm = "the npm in use (before 12) can run a git dependency's prepare script while it resolves dependencies with the network, even under --ignore-scripts; "

	area, err := openDepFetchArea(projectDir)
	if err != nil {
		return refuseFetch(nil, none, "makewand cannot prepare the networked npm fetch outside the project ("+err.Error()+"); install the project's dependencies outside makewand")
	}
	defer area.close()
	dir, err := area.newFetchDir()
	if err != nil {
		return nil, none, err
	}
	defer dir.remove()
	ws, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, none, err
	}
	defer ws.Close()

	hasManifest, lockName := false, ""
	for _, name := range []string{"package.json", "npm-shrinkwrap.json", "package-lock.json"} {
		if name == "package-lock.json" && lockName != "" {
			break // npm reads no package-lock.json beside a shrinkwrap
		}
		err := copyIntoFetchDir(ws, name, dir, maxNpmManifestBytes)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return refuseFetch(nil, none, "makewand cannot copy the project's "+name+" for the networked npm fetch ("+err.Error()+"); "+npmRegistryOnlyRemedy)
		case name == "package.json":
			hasManifest = true
		default:
			lockName = name
		}
	}

	npm := fetchToolPath("npm", projectDir)
	settings := append(slices.Clone(plan.env), "npm_config_ignore_scripts=true", "npm_config_allow_git=none",
		"npm_config_cache="+area.cacheDir(fetchCacheNpm), "npm_config_update_notifier=false")
	spec := fetchSpec{workspace: projectDir, dir: dir.path, cache: area.cache, pathEnv: fetchPathEnv(projectDir), env: settings}
	ci := plan.Args[0] == "ci" || plan.Args[0] == "clean-install"
	keepLock := !ci && !slices.Contains(plan.Args, "--no-package-lock") && !slices.Contains(plan.Args, "--package-lock=false")

	if hasManifest && !ci {
		networked := npmVersionProbe(npm) >= npmNetworkedLockMajor
		if !networked && lockName == "" {
			return refuseFetch(nil, none, "the project has no package-lock.json, and "+oldNpm+"commit a lockfile (npm install --package-lock-only) or use npm 12 or later; "+npmRegistryOnlyRemedy)
		}
		resolve := []string{"install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund"}
		if !networked {
			resolve = append(resolve, "--offline")
		}
		result, network, attached, err := p.execFetchCommand(ctx, env, spec, npm, resolve, networked)
		if !attached && err == nil {
			return p.npmFetchFallback(ctx, env, projectDir, plan, readOnly, network)
		}
		if err != nil {
			return result, network, err
		}
		if result != nil && result.ExitCode != 0 {
			// The resolution runs no project code, so npm's report is its own.
			if reason := npmAllowRefusal(result); reason != "" {
				return refuseFetch(result, network, reason+"; "+npmRegistryOnlyRemedy)
			}
			if !networked && npmNotCached(result) {
				return refuseFetch(result, network, "the project's "+lockName+" does not hold every dependency, so npm would resolve the rest with the network, and "+oldNpm+"update the lockfile (npm install --package-lock-only) or use npm 12 or later; "+npmRegistryOnlyRemedy)
			}
			return result, network, nil // a failed fetch: the outage check or the candidate
		}
		if lockName == "" {
			lockName = "package-lock.json"
		}
	}
	var lock []byte
	if lockName != "" {
		if lock, err = dir.readFile(lockName, maxNpmManifestBytes); err != nil {
			return refuseFetch(nil, none, "npm left no "+lockName+" makewand could inspect ("+err.Error()+"); "+npmRegistryOnlyRemedy)
		}
		dep, err := npmLockfileNonRegistry(lock)
		if err != nil {
			return refuseFetch(nil, none, "makewand cannot inspect the "+lockName+" npm resolved ("+err.Error()+"); "+npmRegistryOnlyRemedy)
		}
		if dep != "" {
			return refuseFetch(nil, none, "the project's dependencies take "+dep+" instead of the registry, which npm before 12 prepares by running its scripts even under --ignore-scripts when it is a git repository or a directory; "+npmRegistryOnlyRemedy)
		}
	}
	install := npmInstallArgs(plan.Args, hasManifest)
	result, network, attached, err := p.execFetchCommand(ctx, env, spec, npm, install, true)
	if !attached && err == nil {
		return p.npmFetchFallback(ctx, env, projectDir, plan, readOnly, network)
	}
	if err != nil || result == nil {
		return result, network, err
	}
	if result.ExitCode != 0 {
		if reason := npmAllowRefusal(result); reason != "" {
			return refuseFetch(result, network, reason+"; "+npmRegistryOnlyRemedy)
		}
		return result, network, nil
	}
	if err := placeFetchTree(dir, ws, "node_modules"); err != nil {
		return result, network, err
	}
	if keepLock && lock != nil {
		if err := writeBackWorkspaceFile(ws, lockName, lock); err != nil {
			return result, network, fmt.Errorf("put the %s npm resolved into the project: %w", lockName, err)
		}
	}
	return result, network, nil
}

// npmFetchFallback runs an npm plan whose user-mode network could not be set
// up the way a dependency step without outbound network runs: as planned,
// loopback-only in the workspace, which explains the stage that failed if a
// download was needed.
func (p *Project) npmFetchFallback(ctx context.Context, env verifyExecEnvironment, projectDir string, plan ExecPlan, readOnly []string, network sandboxNetwork) (*ExecResult, sandboxNetwork, error) {
	result, err := p.execLoopbackStep(ctx, env, projectDir, plan, readOnly, true)
	return result, network, err
}

// npmInstallArgs is the networked install of an npm plan: `npm ci` of the
// inspected lockfile (npm install with neither manifest nor lockfile keeps its
// own subcommand), without scripts, audit or funding requests, keeping the
// plan's options that choose what to install (npmNetworkedOption) but none
// that would change the lockfile.
func npmInstallArgs(planArgs []string, hasManifest bool) []string {
	sub := "ci"
	if !hasManifest {
		sub = planArgs[0]
	}
	args := []string{sub, "--ignore-scripts", "--no-audit", "--no-fund"}
	for _, arg := range planArgs[1:] {
		switch {
		case slices.Contains(args, arg), arg == "--no-package-lock", strings.HasPrefix(arg, "--package-lock="),
			arg == "--no-save", strings.HasPrefix(arg, "--save="):
			continue
		}
		args = append(args, arg)
	}
	return args
}

// npmNetworkedFlags are the npm options without a value a networked fetch
// accepts: they choose what to install or how npm reports, never what to fetch
// from where.
var npmNetworkedFlags = map[string]bool{
	"--ignore-scripts": true, "--no-audit": true, "--no-fund": true, "--no-package-lock": true, "--no-save": true,
	"--production": true, "--no-optional": true, "--legacy-peer-deps": true, "--strict-peer-deps": true,
	"--prefer-offline": true, "--prefer-online": true, "--no-progress": true, "--no-color": true,
	"--silent": true, "--quiet": true, "--verbose": true, "-s": true, "-q": true, "-d": true,
}

// npmNetworkedValueOptions are the npm options a networked fetch accepts as
// --key=value.
var npmNetworkedValueOptions = map[string]bool{
	"--omit": true, "--include": true, "--loglevel": true, "--audit": true, "--fund": true,
	"--package-lock": true, "--save": true, "--progress": true, "--color": true, "--ignore-scripts": true,
}

// npmNetworkedOption reports whether a networked npm fetch accepts arg.
func npmNetworkedOption(arg string) bool {
	if npmNetworkedFlags[arg] {
		return true
	}
	key, _, ok := strings.Cut(arg, "=")
	return ok && npmNetworkedValueOptions[key]
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

// maxWheelMetadataBytes bounds the METADATA makewand reads from a local wheel.
const maxWheelMetadataBytes = 4 << 20

// pipParenSpecifierRe matches a requirement whose version specifiers are in
// parentheses, `name [extras] (specifiers)`: the PEP 508 form bdist_wheel and
// poetry-core write into METADATA, which the strict parser does not take. The
// specifiers must start with an operator, so a bare version cannot join the
// name.
var pipParenSpecifierRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*\s*(?:\[[^\]]*\])?)\s*\(\s*([<>=!~][^()]*)\)$`)

// pipWheelRequires returns the requirements a local wheel declares for the
// selected extras, read from its METADATA (Requires-Dist) without running any
// code, each in the form the networked `pip download` should get it
// (pipWheelRequirement). The caller runs each through the strict parser, which
// drops anything an index would not serve.
func (p *Project) pipWheelRequires(relPath string, extras []string) []string {
	metadata, err := p.readWheelMetadata(relPath)
	if err != nil {
		return nil
	}
	// pip installs a Requires-Dist whose marker holds with extra set to "" (the
	// wheel itself) or to a selected extra the wheel declares; it ignores an
	// extra the wheel does not declare, with a warning (pip 24 and 26).
	declared := map[string]bool{}
	for _, extra := range wheelMetadataField(metadata, "Provides-Extra") {
		declared[normalizePipName(extra)] = true
	}
	contexts := []string{""}
	for _, extra := range extras {
		if declared[extra] && !slices.Contains(contexts, extra) {
			contexts = append(contexts, extra)
		}
	}
	var reqs []string
	for _, value := range wheelMetadataField(metadata, "Requires-Dist") {
		if req, ok := pipWheelRequirement(value, contexts); ok && !slices.Contains(reqs, req) {
			reqs = append(reqs, req)
		}
	}
	return reqs
}

// readWheelMetadata reads the top-level *.dist-info/METADATA of a project-local
// wheel as a zip entry (no code runs). The wheel is opened through an os.Root
// confined to the project.
func (p *Project) readWheelMetadata(relPath string) (string, error) {
	if _, err := p.validatePath(relPath, false); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.Open(filepath.Clean(relPath))
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", relPath)
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return "", err
	}
	for _, zf := range zr.File {
		if !isWheelMetadataName(zf.Name) {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxWheelMetadataBytes+1))
		rc.Close()
		if err != nil {
			return "", err
		}
		if int64(len(data)) > maxWheelMetadataBytes {
			return "", fmt.Errorf("%s METADATA is larger than %d bytes", relPath, maxWheelMetadataBytes)
		}
		return string(data), nil
	}
	return "", fmt.Errorf("%s has no dist-info METADATA", relPath)
}

// isWheelMetadataName reports whether a zip entry name is a wheel's top-level
// <name>-<version>.dist-info/METADATA (zip names use forward slashes).
func isWheelMetadataName(name string) bool {
	dir, ok := strings.CutSuffix(name, "/METADATA")
	return ok && !strings.Contains(dir, "/") && strings.HasSuffix(dir, ".dist-info")
}

// wheelMetadataField returns every value of a METADATA header (case-insensitive),
// joining folded continuation lines. Only the header block (up to the first
// blank line) is read; the long-description body that follows is ignored.
func wheelMetadataField(metadata, field string) []string {
	var values []string
	var cur strings.Builder
	has := false
	flush := func() {
		if has {
			values = append(values, strings.TrimSpace(cur.String()))
		}
		cur.Reset()
		has = false
	}
	prefix := strings.ToLower(field) + ":"
	for _, line := range strings.Split(metadata, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if has {
				cur.WriteString(" ")
				cur.WriteString(strings.TrimSpace(line))
			}
			continue
		}
		flush()
		if rest, ok := cutHeaderPrefix(line, prefix); ok {
			cur.WriteString(strings.TrimSpace(rest))
			has = true
		}
	}
	flush()
	return values
}

// cutHeaderPrefix returns the value after a case-insensitive header prefix.
func cutHeaderPrefix(line, lowerPrefix string) (string, bool) {
	if len(line) < len(lowerPrefix) || !strings.EqualFold(line[:len(lowerPrefix)], lowerPrefix) {
		return "", false
	}
	return line[len(lowerPrefix):], true
}

// pipWheelRequirement prepares a wheel's Requires-Dist value for the networked
// `pip download`, which evaluates a command-line requirement's marker with no
// extra selected. makewand therefore decides the marker's extra comparisons
// itself, once per extra context (pipWheelRequires), and hands pip only what is
// left: the requirement alone when the marker holds outright, its environment
// clauses (python_version, sys_platform, ...) when they decide, nothing when it
// never holds. The extra comparisons are never replaced by constant
// comparisons: pip cannot evaluate a comparison of two strings (confirmed on
// pip 24 and 26) and the whole download would fail. A marker with no extra
// comparison, or one makewand cannot read, is kept as written for pip.
func pipWheelRequirement(value string, contexts []string) (string, bool) {
	req, marker, hasMarker := strings.Cut(value, ";")
	req = strings.TrimSpace(req)
	if m := pipParenSpecifierRe.FindStringSubmatch(req); m != nil {
		req = strings.TrimSpace(m[1]) + strings.TrimSpace(m[2])
	}
	marker = strings.TrimSpace(marker)
	switch {
	case req == "":
		return "", false
	case !hasMarker || marker == "":
		return req, true
	}
	expr, ok := parsePipMarker(marker)
	if !ok || !expr.mentionsExtra() {
		return req + " ; " + marker, true
	}
	var residuals []*pipMarker
	seen := map[string]bool{}
	for _, extra := range contexts {
		residual, holds, ok := expr.withExtra(extra)
		switch {
		case !ok:
			return req + " ; " + marker, true
		case residual == nil && holds:
			return req, true
		case residual != nil && !seen[residual.String()]:
			seen[residual.String()] = true
			residuals = append(residuals, residual)
		}
	}
	switch len(residuals) {
	case 0:
		return "", false
	case 1:
		return req + " ; " + residuals[0].String(), true
	}
	return req + " ; " + (&pipMarker{op: "or", terms: residuals}).String(), true
}

// pipMarker is a parsed PEP 508 environment marker: a comparison, or the "and"
// or "or" of its terms.
type pipMarker struct {
	op       string // "and", "or", or the comparison operator
	terms    []*pipMarker
	lhs, rhs pipMarkerOperand
}

// pipMarkerOperand is a comparison operand: a variable, or a quoted string.
type pipMarkerOperand struct {
	text     string // as written, a string with its quotes
	value    string // a string's content
	variable bool
}

// maxPipMarkerDepth bounds the parenthesis nesting parsePipMarker accepts, so
// a crafted METADATA cannot exhaust the stack.
const maxPipMarkerDepth = 32

// pipMarkerVariables are the marker variables packaging accepts, including
// its legacy dotted spellings.
var pipMarkerVariables = map[string]bool{
	"python_version": true, "python_full_version": true, "os_name": true, "os.name": true,
	"sys_platform": true, "sys.platform": true, "platform_release": true, "platform_system": true,
	"platform_version": true, "platform.version": true, "platform_machine": true, "platform.machine": true,
	"platform_python_implementation": true, "platform.python_implementation": true,
	"python_implementation": true, "implementation_name": true, "implementation_version": true,
	"extra": true,
}

// pipMarkerOperators are the comparison operators, longest first.
var pipMarkerOperators = []string{"===", "==", "~=", "!=", "<=", ">=", "<", ">"}

// parsePipMarker parses an environment marker as packaging does: "and" binds
// tighter than "or", parentheses group, and a comparison sets a variable or a
// quoted string against another with a version operator, in or not in.
func parsePipMarker(text string) (*pipMarker, bool) {
	tokens, ok := pipMarkerTokens(text)
	if !ok {
		return nil, false
	}
	parser := pipMarkerParser{tokens: tokens}
	marker, ok := parser.or(0)
	return marker, ok && parser.pos == len(tokens)
}

// pipMarkerTokens splits a marker into parentheses, quoted strings (PEP 508
// has no escapes), operators, the keywords and, or, not and in, and variables.
func pipMarkerTokens(text string) ([]string, bool) {
	var tokens []string
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(' || c == ')':
			tokens = append(tokens, text[i:i+1])
			i++
		case c == '"' || c == '\'':
			end := strings.IndexByte(text[i+1:], c)
			if end < 0 {
				return nil, false
			}
			tokens = append(tokens, text[i:i+end+2])
			i += end + 2
		case strings.IndexByte("=!~<>", c) >= 0:
			j := slices.IndexFunc(pipMarkerOperators, func(op string) bool { return strings.HasPrefix(text[i:], op) })
			if j < 0 {
				return nil, false
			}
			tokens = append(tokens, pipMarkerOperators[j])
			i += len(pipMarkerOperators[j])
		default:
			j := i
			for j < len(text) && (text[j] == '_' || text[j] == '.' || 'a' <= text[j] && text[j] <= 'z' || 'A' <= text[j] && text[j] <= 'Z' || '0' <= text[j] && text[j] <= '9') {
				j++
			}
			switch word := text[i:j]; {
			case pipMarkerVariables[word], word == "and", word == "or", word == "not", word == "in":
				tokens = append(tokens, word)
			default:
				return nil, false
			}
			i = j
		}
	}
	return tokens, true
}

// pipMarkerParser is a recursive-descent parser over pipMarkerTokens.
type pipMarkerParser struct {
	tokens []string
	pos    int
}

// next consumes and returns the next token ("" at the end).
func (p *pipMarkerParser) next() string {
	if p.pos == len(p.tokens) {
		return ""
	}
	p.pos++
	return p.tokens[p.pos-1]
}

// or parses `and ("or" and)*`.
func (p *pipMarkerParser) or(depth int) (*pipMarker, bool) {
	return p.list("or", depth, p.and)
}

// and parses `atom ("and" atom)*`.
func (p *pipMarkerParser) and(depth int) (*pipMarker, bool) {
	return p.list("and", depth, p.atom)
}

// list parses `term (op term)*`.
func (p *pipMarkerParser) list(op string, depth int, term func(int) (*pipMarker, bool)) (*pipMarker, bool) {
	first, ok := term(depth)
	if !ok {
		return nil, false
	}
	terms := []*pipMarker{first}
	for p.pos < len(p.tokens) && p.tokens[p.pos] == op {
		p.pos++
		next, ok := term(depth)
		if !ok {
			return nil, false
		}
		terms = append(terms, next)
	}
	if len(terms) == 1 {
		return first, true
	}
	return &pipMarker{op: op, terms: terms}, true
}

// atom parses a parenthesized marker or a comparison.
func (p *pipMarkerParser) atom(depth int) (*pipMarker, bool) {
	if p.pos < len(p.tokens) && p.tokens[p.pos] == "(" {
		if depth == maxPipMarkerDepth {
			return nil, false
		}
		p.pos++
		inner, ok := p.or(depth + 1)
		if !ok || p.next() != ")" {
			return nil, false
		}
		return inner, true
	}
	lhs, ok := p.operand()
	if !ok {
		return nil, false
	}
	op := p.next()
	switch {
	case op == "not":
		if p.next() != "in" {
			return nil, false
		}
		op = "not in"
	case op != "in" && !slices.Contains(pipMarkerOperators, op):
		return nil, false
	}
	rhs, ok := p.operand()
	if !ok {
		return nil, false
	}
	return &pipMarker{op: op, lhs: lhs, rhs: rhs}, true
}

// operand parses a variable or a quoted string.
func (p *pipMarkerParser) operand() (pipMarkerOperand, bool) {
	switch tok := p.next(); {
	case pipMarkerVariables[tok]:
		return pipMarkerOperand{text: tok, variable: true}, true
	case tok != "" && (tok[0] == '"' || tok[0] == '\''):
		return pipMarkerOperand{text: tok, value: tok[1 : len(tok)-1]}, true
	}
	return pipMarkerOperand{}, false
}

// isExtra reports whether the operand is the extra variable.
func (o pipMarkerOperand) isExtra() bool {
	return o.variable && o.text == "extra"
}

// mentionsExtra reports whether any comparison of the marker involves extra.
func (m *pipMarker) mentionsExtra() bool {
	if m.op == "and" || m.op == "or" {
		return slices.ContainsFunc(m.terms, (*pipMarker).mentionsExtra)
	}
	return m.lhs.isExtra() || m.rhs.isExtra()
}

// withExtra decides the marker's extra comparisons for one extra context (a
// normalized name, "" for none) and simplifies what is left. It returns the
// residual marker, or nil and the marker's value when nothing else decides it.
// ok is false for an extra comparison makewand does not decide: one against a
// variable, or with an operator other than ==, !=, in and not in.
func (m *pipMarker) withExtra(extra string) (residual *pipMarker, holds, ok bool) {
	if m.op != "and" && m.op != "or" {
		return m.compareExtra(extra)
	}
	// One false term decides an "and", one true term an "or".
	decisive := m.op == "or"
	decided := false
	var rest []*pipMarker
	for _, term := range m.terms {
		residual, holds, ok := term.withExtra(extra)
		switch {
		case !ok:
			return nil, false, false
		case residual != nil:
			rest = append(rest, residual)
		case holds == decisive:
			decided = true
		}
	}
	switch {
	case decided:
		return nil, decisive, true
	case len(rest) == 0:
		return nil, !decisive, true
	case len(rest) == 1:
		return rest[0], false, true
	}
	return &pipMarker{op: m.op, terms: rest}, false, true
}

// compareExtra decides a comparison of extra with a string the way packaging
// does, both names normalized (PEP 685); any other comparison is left to pip.
// pip before 24.1 (packaging 21.3) compares the names as written: for ==, the
// comparison wheels write, it then installs no more than was fetched.
func (m *pipMarker) compareExtra(extra string) (*pipMarker, bool, bool) {
	lhsExtra, rhsExtra := m.lhs.isExtra(), m.rhs.isExtra()
	switch {
	case !lhsExtra && !rhsExtra:
		return m, false, true
	case lhsExtra && m.rhs.variable, rhsExtra && m.lhs.variable:
		return nil, false, false
	}
	lhs, rhs := extra, normalizePipName(m.rhs.value)
	if rhsExtra {
		lhs, rhs = normalizePipName(m.lhs.value), extra
	}
	switch m.op {
	case "==":
		return nil, lhs == rhs, true
	case "!=":
		return nil, lhs != rhs, true
	case "in":
		return nil, strings.Contains(rhs, lhs), true
	case "not in":
		return nil, !strings.Contains(rhs, lhs), true
	}
	return nil, false, false
}

// String writes the marker in PEP 508 syntax: comparisons as written, nested
// "and"/"or" terms in parentheses.
func (m *pipMarker) String() string {
	if m.op != "and" && m.op != "or" {
		return m.lhs.text + " " + m.op + " " + m.rhs.text
	}
	terms := make([]string, len(m.terms))
	for i, term := range m.terms {
		terms[i] = term.String()
		if term.op == "and" || term.op == "or" {
			terms[i] = "(" + terms[i] + ")"
		}
	}
	return strings.Join(terms, " "+m.op+" ")
}
