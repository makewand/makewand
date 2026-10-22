package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// pipDependencyStages splits a `pip install` into a `pip download` of wheels
// only (no sdist builds, so no project code runs with the network) into a
// wheelhouse in the fetch directory, then an offline `pip install --no-index
// --find-links <wheelhouse>` that builds any local project and installs from
// what was fetched, with the wheelhouse bound read-only.
func (p *Project) pipDependencyStages(plan ExecPlan, ws *os.Root, stage *fetchStage) (fetch, build *ExecPlan, err error) {
	install, ok := p.parsePipInstall(plan.Args)
	if !ok {
		b := plan
		return nil, &b, nil
	}
	if err := stage.dir.mkdirAll("wheelhouse"); err != nil {
		return nil, nil, err
	}
	wheelhouse := stage.dir.abs("wheelhouse")
	buildPlan := plan
	buildPlan.Args = pipOfflineInstallArgs(plan.Args, wheelhouse)
	buildPlan.binds = []string{wheelhouse}
	sources, sourceArgs, err := p.pipFetchSourceArgs(install, ws, stage.dir)
	if err != nil {
		return nil, nil, err
	}
	stage.pip = sources
	fetchArgs, hasTargets := p.pipDownloadArgs(install, wheelhouse, sourceArgs)
	if !hasTargets {
		// Nothing the install resolves from a package index (for example `-e .`
		// with no declared dependencies): there is nothing to download, so no
		// networked step runs at all - the offline build installs the local
		// project from source loopback-only.
		return nil, &buildPlan, nil
	}
	// Pin the fetch to the install's constraints, sanitised to named pins only,
	// so the wheels downloaded are the versions the offline install resolves.
	if len(install.constraintPins) > 0 {
		if err := stage.dir.writeFile("constraints.txt", []byte(strings.Join(install.constraintPins, "\n")+"\n"), 0o600); err != nil {
			return nil, nil, err
		}
		fetchArgs = append(fetchArgs, "--constraint", stage.dir.abs("constraints.txt"))
	}
	return &ExecPlan{Kind: "deps", Detector: plan.Detector, Command: plan.Command, Args: fetchArgs, env: slices.Clone(plan.env)}, &buildPlan, nil
}

// pipDownloadArgs builds the `pip download` command line for a parsed install:
// wheels only (--only-binary=:all:) into the wheelhouse, from the package
// sources sourceArgs names. It passes only the named index requirements the
// install resolves - those from the command line and from the requirement
// files makewand can read, and, for each local project the install builds,
// its build-system requires, its statically declared dependencies and the
// optional-dependency groups its extras select (what a setup.py computes at
// build time cannot be known without running it). It never passes a
// requirement file (-r) or constraint file (-c), nor any buildable target (a
// local project, an sdist, a VCS reference, a URL, or a name @ url direct
// reference): pip builds such a target to read its metadata even under
// --only-binary=:all: (confirmed on pip 24 and 26), which would run project
// code while the network is available. The offline build installs the local
// project from the wheelhouse instead, loopback-only. hasTargets reports
// whether anything resolves from an index at all.
func (p *Project) pipDownloadArgs(install pipInstall, wheelhouse string, sourceArgs []string) (args []string, hasTargets bool) {
	var reqs []string
	add := func(texts ...string) {
		for _, text := range texts {
			if named, ok := pipNamedRequirementText(text); ok {
				reqs = append(reqs, named)
			}
		}
	}
	add(install.downloadReqs...)
	for _, project := range install.projects {
		add(p.pipBuildSystemRequires(project.dir)...)
		// The declared texts, markers kept: pipStaticDependencies drops a
		// requirement with a marker, which the offline install still needs
		// whenever the marker holds.
		texts, _ := p.pipStaticDependencyTexts(project.dir)
		add(texts...)
		optional, _ := p.pipOptionalDependencyTexts(project)
		add(optional...)
	}
	for _, wheel := range install.wheels {
		// A wheel runs no code; its Requires-Dist are read from METADATA
		// (a zip read) and fetched so the offline install can resolve them.
		add(p.pipWheelRequires(wheel.dir, wheel.extras)...)
	}
	if len(reqs) == 0 {
		return nil, false
	}
	args = []string{"download", "--only-binary=:all:", "--no-input", "--dest", wheelhouse}
	args = append(args, sourceArgs...)
	args = append(args, reqs...)
	return args, true
}

// pipFetchSources are the package sources a pip fetch used, for asking its
// indexes about a requirement pip found no distribution for.
type pipFetchSources struct {
	index   string // "" for pip's default, PyPI
	extra   []string
	noIndex bool
}

// pipDefaultIndex is the index pip uses when nothing names another one.
const pipDefaultIndex = "https://pypi.org/simple/"

// indexes returns the indexes the fetch searched.
func (s pipFetchSources) indexes() []string {
	if s.noIndex {
		return nil
	}
	index := s.index
	if index == "" {
		index = pipDefaultIndex
	}
	return append([]string{index}, s.extra...)
}

// Limits of the local wheels makewand copies into a fetch directory.
const (
	maxFetchWheelBytes      = 1 << 30
	maxFetchWheelTotalBytes = 4 << 30
)

// pipFetchSourceArgs returns the package source options of the networked
// `pip download`: the install's own (command line and requirement files)
// over the pip configuration the project and the sandbox's pip would read
// (pipConfigSources). An http(s) index, extra index or find-links URL and a
// trusted host are forwarded as they are; a local find-links directory inside
// the workspace has its wheels copied into the fetch directory (as regular
// files, through the workspace's os.Root handle) and is forwarded as that
// copy. A configured index is used, never silently replaced by PyPI: an index
// or find-links location the fetch cannot be given (not http(s), or a
// directory outside the workspace) refuses the networked fetch.
func (p *Project) pipFetchSourceArgs(install pipInstall, ws *os.Root, dir *fetchDir) (pipFetchSources, []string, error) {
	cfg := p.pipConfigSources()
	opts := install.sources
	if opts.index == "" {
		opts.index = cfg.index
	}
	opts.extra = append(slices.Clone(cfg.extra), opts.extra...)
	opts.trusted = append(slices.Clone(cfg.trusted), opts.trusted...)
	opts.findLinks = append(slices.Clone(cfg.findLinks), opts.findLinks...)
	opts.noIndex = opts.noIndex || cfg.noIndex

	refuse := func(what, value string) (pipFetchSources, []string, error) {
		return pipFetchSources{}, nil, &fetchRefusal{reason: "the pip install uses the " + what + " " + truncateForNote(redactURL(value)) + ", which makewand cannot hand to the networked fetch (it forwards http(s) indexes and wheel directories inside the project); makewand never replaces a configured package source with PyPI, so install the project's dependencies outside makewand"}
	}
	sources := pipFetchSources{noIndex: opts.noIndex}
	var args []string
	if opts.noIndex {
		args = append(args, "--no-index")
	} else {
		if opts.index != "" {
			if !pipHTTPURL(opts.index) {
				return refuse("package index", opts.index)
			}
			sources.index = opts.index
			args = append(args, "--index-url", opts.index)
		}
		for _, extra := range opts.extra {
			if !pipHTTPURL(extra) {
				return refuse("extra package index", extra)
			}
			if !slices.Contains(sources.extra, extra) {
				sources.extra = append(sources.extra, extra)
				args = append(args, "--extra-index-url", extra)
			}
		}
	}
	for _, host := range opts.trusted {
		args = append(args, "--trusted-host", host)
	}
	copied := map[string]bool{}
	var total int64
	for _, link := range opts.findLinks {
		if pipHTTPURL(link.value) {
			args = append(args, "--find-links", link.value)
			continue
		}
		rel, ok := p.pipLocalFindLinks(link)
		if !ok {
			return refuse("find-links location", link.value)
		}
		if !copied[rel] {
			copied[rel] = true
			n, err := copyWheelDir(ws, rel, dir, &total)
			if err != nil {
				return pipFetchSources{}, nil, &fetchRefusal{reason: "makewand cannot copy the wheels of the find-links directory " + truncateForNote(link.value) + " for the networked fetch (" + err.Error() + "); install the project's dependencies outside makewand"}
			}
			if n == 0 && rel != "." {
				_ = dir.mkdirAll(rel)
			}
		}
		args = append(args, "--find-links", dir.abs(rel))
	}
	return sources, args, nil
}

// pipLocalFindLinks resolves a local find-links location to a directory
// relative to the workspace: a path relative to the requirement file that
// names it when it exists there (as pip resolves it), else to the workspace
// root, an absolute path or a file: URL inside the workspace. false for one
// outside the workspace.
func (p *Project) pipLocalFindLinks(link pipFindLinks) (string, bool) {
	projectDir, err := p.execWorkingDir()
	if err != nil {
		return "", false
	}
	value := link.value
	if u, err := url.Parse(value); err == nil && strings.EqualFold(u.Scheme, "file") {
		value = u.Path
	}
	var candidates []string
	switch {
	case filepath.IsAbs(value):
		candidates = append(candidates, value)
	default:
		if link.base != "" && link.base != "." {
			candidates = append(candidates, filepath.Join(projectDir, link.base, value))
		}
		candidates = append(candidates, filepath.Join(projectDir, value))
	}
	for i, candidate := range candidates {
		rel, err := filepath.Rel(projectDir, filepath.Clean(candidate))
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if i < len(candidates)-1 {
			if info, err := os.Stat(candidate); err != nil || !info.IsDir() {
				continue // pip falls back to the path relative to the working directory
			}
		}
		return rel, true
	}
	return "", false
}

// copyWheelDir copies the regular *.whl files of the workspace directory rel
// (not its subdirectories, as pip lists a find-links directory) into the same
// path of the fetch directory, within the size limits. A missing directory
// copies nothing.
func copyWheelDir(ws *os.Root, rel string, dst *fetchDir, total *int64) (int, error) {
	if rel != "." {
		// Every component must be a directory: a symlink is refused, never
		// followed.
		parts := strings.Split(rel, string(filepath.Separator))
		for i := range parts {
			component := filepath.Join(parts[:i+1]...)
			info, err := ws.Lstat(component)
			if errors.Is(err, fs.ErrNotExist) {
				return 0, nil
			}
			if err != nil {
				return 0, err
			}
			if !info.IsDir() {
				return 0, errors.New(component + " is not a directory")
			}
		}
	}
	entries, err := fs.ReadDir(ws.FS(), filepath.ToSlash(rel))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".whl") {
			continue
		}
		file := filepath.Join(rel, entry.Name())
		data, err := readRegularNoFollow(ws, file, maxFetchWheelBytes)
		if err != nil {
			return n, err
		}
		if *total += int64(len(data)); *total > maxFetchWheelTotalBytes {
			return n, errors.New("the local wheels exceed the copy limit")
		}
		if err := dst.writeFile(file, data, 0o600); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// pipConfigSources returns the package source settings of the pip
// configuration files the sandbox's pip reads for an install, and of the
// project's own pip.conf or pip.ini: site-wide ones, those in the sandbox HOME
// (both read statically, never run), the trusted pip's site configuration,
// and the project's, each overriding the ones before it; within a file the
// [install] section overrides [global].
func (p *Project) pipConfigSources() pipSourceOptions {
	var cfg pipSourceOptions
	for _, content := range p.pipConfigFileContents() {
		values := pipConfigValues(content)
		if v, ok := values["index-url"]; ok {
			cfg.index = strings.TrimSpace(v)
		}
		if v, ok := values["extra-index-url"]; ok {
			cfg.extra = strings.Fields(v)
		}
		if v, ok := values["trusted-host"]; ok {
			cfg.trusted = slices.DeleteFunc(strings.Fields(v), func(h string) bool { return !pipTrustedHost(h) })
		}
		if v, ok := values["find-links"]; ok {
			cfg.findLinks = nil
			for _, link := range strings.Fields(v) {
				cfg.findLinks = append(cfg.findLinks, pipFindLinks{value: link, base: "."})
			}
		}
		if v, ok := values["no-index"]; ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.noIndex = true
			case "0", "false", "no", "off":
				cfg.noIndex = false
			}
		}
	}
	return cfg
}

// pipHostConfigFiles are the site-wide pip configuration files, lowest
// precedence first. A package variable so tests keep the host's out.
var pipHostConfigFiles = []string{"/etc/xdg/pip/pip.conf", "/etc/pip.conf"}

// pipConfigFileContents returns the contents of the pip configuration files
// pipConfigSources reads, lowest precedence first. Files in the workspace are
// read through its os.Root handle and never through a symlink.
func (p *Project) pipConfigFileContents() []string {
	var contents []string
	for _, file := range pipHostConfigFiles {
		if data, err := os.ReadFile(file); err == nil { //nolint:gosec // G304: fixed pip configuration paths
			contents = append(contents, string(data))
		}
	}
	projectDir, err := p.execWorkingDir()
	if err != nil {
		return contents
	}
	ws, err := os.OpenRoot(projectDir)
	if err != nil {
		return contents
	}
	defer ws.Close()
	home := filepath.FromSlash(sandboxHomeRelPath)
	for _, rel := range []string{filepath.Join(home, ".pip", "pip.conf"), filepath.Join(home, ".config", "pip", "pip.conf")} {
		if data, err := readRegularNoFollow(ws, rel, maxReadFileSize); err == nil {
			contents = append(contents, string(data))
		}
	}
	if python := pipFetchInterpreter(projectDir); python != "" {
		site := filepath.Join(filepath.Dir(filepath.Dir(python)), "pip.conf")
		if data, err := os.ReadFile(site); err == nil { //nolint:gosec // G304: the trusted pip's own site configuration
			contents = append(contents, string(data))
		}
	}
	for _, rel := range []string{"pip.conf", "pip.ini"} {
		if data, err := readRegularNoFollow(ws, rel, maxReadFileSize); err == nil {
			contents = append(contents, string(data))
		}
	}
	return contents
}

// pipConfigValues reads a pip configuration file (configparser syntax: [section]
// headers, key = value or key: value, indented continuation lines, # and ;
// comments) and returns the values an install uses: [global], overridden by
// [install]. Keys are normalized as pip normalizes them.
func pipConfigValues(content string) map[string]string {
	sections := map[string]map[string]string{}
	section, key := "", ""
	for _, raw := range strings.Split(content, "\n") {
		raw = strings.TrimRight(raw, "\r")
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case raw[0] == ' ' || raw[0] == '\t':
			if key != "" && sections[section] != nil {
				sections[section][key] += "\n" + line
			}
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section, key = strings.ToLower(strings.TrimSpace(line[1:len(line)-1])), ""
			continue
		}
		i := strings.IndexAny(line, "=:")
		if i < 0 || section == "" {
			key = ""
			continue
		}
		key = strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(line[:i]), "--")), "_", "-")
		if sections[section] == nil {
			sections[section] = map[string]string{}
		}
		sections[section][key] = strings.TrimSpace(line[i+1:])
	}
	values := map[string]string{}
	for _, name := range []string{"global", "install"} {
		for k, v := range sections[name] {
			values[k] = v
		}
	}
	return values
}

// pipFetchInterpreter returns the Python interpreter of the pip the fetch
// runs (fetchToolPath), or of the fetch PATH's python3, or "".
func pipFetchInterpreter(projectDir string) string {
	pathEnv := fetchPathEnv(projectDir)
	for _, name := range []string{"pip", "pip3"} {
		if bin := fetchToolPath(name, projectDir); filepath.IsAbs(bin) {
			if python := scriptInterpreterAny(bin, pathEnv); filepath.IsAbs(python) {
				return python
			}
		}
	}
	if python := fetchToolPath("python3", projectDir); filepath.IsAbs(python) {
		return python
	}
	return ""
}

// pipMarkerEnvironment returns the marker environment of the interpreter
// python, or nil when it cannot be told. A package variable so tests choose
// the environment.
var pipMarkerEnvironment = probePipMarkerEnvironment

var (
	pipMarkerEnvMu    sync.Mutex
	pipMarkerEnvCache = map[string]pipMarkerEnv{}
)

// pipMarkerEnvScript prints packaging's default_environment.
const pipMarkerEnvScript = `import json, os, platform, sys
def fmt(info):
    v = "%d.%d.%d" % (info.major, info.minor, info.micro)
    if info.releaselevel != "final":
        v += info.releaselevel[0] + str(info.serial)
    return v
print(json.dumps({
    "implementation_name": sys.implementation.name,
    "implementation_version": fmt(sys.implementation.version),
    "os_name": os.name,
    "platform_machine": platform.machine(),
    "platform_release": platform.release(),
    "platform_system": platform.system(),
    "platform_version": platform.version(),
    "python_full_version": platform.python_version(),
    "platform_python_implementation": platform.python_implementation(),
    "python_version": ".".join(platform.python_version_tuple()[:2]),
    "sys_platform": sys.platform,
}))
`

// probePipMarkerEnvironment runs the trusted interpreter python (never one of
// the workspace: pipFetchInterpreter) isolated (-I -S), from the root
// directory with a minimal environment, to read its marker environment.
func probePipMarkerEnvironment(python string) pipMarkerEnv {
	if python == "" || !filepath.IsAbs(python) {
		return nil
	}
	pipMarkerEnvMu.Lock()
	cached, ok := pipMarkerEnvCache[python]
	pipMarkerEnvMu.Unlock()
	if ok {
		return cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	//nolint:gosec // G204: a trusted interpreter outside the workspace (pipFetchInterpreter); fixed script, no shell.
	cmd := exec.CommandContext(ctx, python, "-I", "-S", "-c", pipMarkerEnvScript)
	cmd.Dir = string(filepath.Separator)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var env pipMarkerEnv
	if json.Unmarshal(out, &env) != nil || env["python_version"] == "" {
		return nil
	}
	pipMarkerEnvMu.Lock()
	pipMarkerEnvCache[python] = env
	pipMarkerEnvMu.Unlock()
	return env
}
