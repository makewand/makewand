package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A failed pip fetch is an environment limitation of fetch-only networking
// only when the package index shows that releases matching the requested
// specifier exist and none of them offers a wheel this interpreter can
// install (they are sdist-only here): makewand downloads wheels only, so no
// project code runs with the network. A project or version the index does not
// have, a release whose wheels pip should have taken, or an answer makewand
// cannot get is the candidate's failure. The index is asked through the
// network the fetch had - makewand's own executable runs as a probe in a
// fresh sandbox with the user-mode network - with its simple repository API
// (PEP 691 JSON, or the PEP 503 HTML page an index without JSON serves). No
// code runs.

// pipIndexProbeArg, as its first argument, makes makewand's own executable
// the package index probe (runPipIndexProbe).
const pipIndexProbeArg = "__makewand-pip-index-probe"

// maxPipIndexPageBytes bounds the project page the probe reads.
const maxPipIndexPageBytes = 32 << 20

// pipIndexFile is one distribution file a project page lists.
type pipIndexFile struct {
	name           string
	yanked         bool
	requiresPython string
}

// pipIndexAnswer is what a package index said about a project page.
type pipIndexAnswer int

const (
	// pipIndexUnknown: no usable answer (no response, an unexpected status,
	// a page makewand cannot read, a probe that could not run).
	pipIndexUnknown pipIndexAnswer = iota
	// pipIndexNotFound: the index has no such project (404/410).
	pipIndexNotFound
	// pipIndexListed: the page was read; its files are returned.
	pipIndexListed
)

// pipIndexFiles lists the files of a project page of a package index through
// the fetch's network. A package variable so tests answer without a network.
var pipIndexFiles = lookupPipIndexFiles

// lookupPipIndexFiles runs the probe for pageURL in a fresh sandbox with the
// user-mode network, from an empty directory.
func lookupPipIndexFiles(ctx context.Context, env verifyExecEnvironment, pageURL, workspace string) ([]pipIndexFile, pipIndexAnswer) {
	if env.mode != verifyExecIsolated {
		return nil, pipIndexUnknown
	}
	probe, err := registryProbeExecutable()
	if err != nil {
		return nil, pipIndexUnknown
	}
	dir, err := os.MkdirTemp("", "makewand-pip-index-*")
	if err != nil {
		return nil, pipIndexUnknown
	}
	defer os.RemoveAll(dir)
	checkCtx, cancel := context.WithTimeout(ctx, registryCheckTimeout)
	defer cancel()
	run := runRegistryProbeInSandbox(checkCtx, env.bwrapPath, dir, probe, workspace, []string{pipIndexProbeArg, pageURL})
	if run.failure != "" {
		return nil, pipIndexUnknown
	}
	return parsePipIndexProbe(run.stdout)
}

// parsePipIndexProbe reads the probe's output: "status <code>", then one
// "file <yanked> <requires-python> <name>" line per file (requires-python
// quoted with strconv.Quote).
func parsePipIndexProbe(out string) ([]pipIndexFile, pipIndexAnswer) {
	status := 0
	var files []pipIndexFile
	complete := false
	for _, line := range strings.Split(out, "\n") {
		kind, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch kind {
		case "status":
			status, _ = strconv.Atoi(rest)
		case "file":
			yanked, rest, _ := strings.Cut(rest, " ")
			quoted, name, ok := cutQuoted(rest)
			if !ok || name == "" {
				return nil, pipIndexUnknown
			}
			files = append(files, pipIndexFile{name: name, yanked: yanked == "1", requiresPython: quoted})
		case "end":
			complete = true
		}
	}
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return nil, pipIndexNotFound
	case status == http.StatusOK && complete:
		return files, pipIndexListed
	}
	return nil, pipIndexUnknown
}

// cutQuoted splits a leading Go-quoted string off s.
func cutQuoted(s string) (quoted, rest string, ok bool) {
	prefix, err := strconv.QuotedPrefix(s)
	if err != nil {
		return "", "", false
	}
	value, err := strconv.Unquote(prefix)
	if err != nil {
		return "", "", false
	}
	return value, strings.TrimSpace(s[len(prefix):]), true
}

// runPipIndexProbe fetches the project page args[0] and prints what
// parsePipIndexProbe reads. Requests use verified TLS and the proxy settings
// of the environment (the sandbox clears them, as it does for the fetch).
func runPipIndexProbe(out io.Writer, args []string) int {
	if len(args) != 1 {
		return 2
	}
	client := &http.Client{Timeout: registryProbeRequestTimeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: registryProbeRequestTimeout}}
	// The URL is a package index the fetch used, from the pip command line or
	// configuration makewand forwarded to it, asked from the same network.
	req, err := http.NewRequest(http.MethodGet, args[0], nil) // #nosec G704
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "makewand-pip-index-check")
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json, text/html;q=0.01")
	resp, err := client.Do(req) // #nosec G704
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = fmt.Fprintln(out, "status", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPipIndexPageBytes+1))
	if err != nil || len(body) > maxPipIndexPageBytes {
		return 0
	}
	var files []pipIndexFile
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		var ok bool
		if files, ok = pipIndexJSONFiles(body); !ok {
			return 0
		}
	} else {
		files = pipIndexHTMLFiles(string(body))
	}
	for _, f := range files {
		yanked := "0"
		if f.yanked {
			yanked = "1"
		}
		_, _ = fmt.Fprintln(out, "file", yanked, strconv.Quote(f.requiresPython), f.name)
	}
	_, _ = fmt.Fprintln(out, "end")
	return 0
}

// pipIndexJSONFiles reads a PEP 691 project page.
func pipIndexJSONFiles(body []byte) ([]pipIndexFile, bool) {
	var page struct {
		Files []struct {
			Filename       string          `json:"filename"`
			Yanked         json.RawMessage `json:"yanked"`
			RequiresPython string          `json:"requires-python"`
		} `json:"files"`
	}
	if json.Unmarshal(body, &page) != nil {
		return nil, false
	}
	var files []pipIndexFile
	for _, f := range page.Files {
		if strings.ContainsAny(f.Filename, " \t\r\n/") || f.Filename == "" {
			continue
		}
		yanked := false
		var flag bool
		var reason string
		switch {
		case json.Unmarshal(f.Yanked, &flag) == nil:
			yanked = flag
		case json.Unmarshal(f.Yanked, &reason) == nil:
			yanked = true
		}
		files = append(files, pipIndexFile{name: f.Filename, yanked: yanked, requiresPython: f.RequiresPython})
	}
	return files, true
}

var (
	pipIndexAnchorRe = regexp.MustCompile(`(?is)<a\s([^>]*)>(.*?)</a>`)
	pipIndexAttrRe   = regexp.MustCompile(`(?is)([a-z-]+)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?`)
)

// pipIndexHTMLFiles reads a PEP 503 project page: each anchor's text is a file
// name, data-yanked marks a yanked file, data-requires-python its Python
// requirement.
func pipIndexHTMLFiles(body string) []pipIndexFile {
	var files []pipIndexFile
	for _, m := range pipIndexAnchorRe.FindAllStringSubmatch(body, -1) {
		name := strings.TrimSpace(html.UnescapeString(m[2]))
		if name == "" || strings.ContainsAny(name, " \t\r\n/<>") {
			continue
		}
		file := pipIndexFile{name: name}
		for _, attr := range pipIndexAttrRe.FindAllStringSubmatch(m[1], -1) {
			value := html.UnescapeString(attr[2] + attr[3] + attr[4])
			switch strings.ToLower(attr[1]) {
			case "data-yanked":
				file.yanked = true
			case "data-requires-python":
				file.requiresPython = value
			}
		}
		files = append(files, file)
	}
	return files
}

// pipReleaseVerdict is what the index showed about a requirement pip found
// no distribution for.
type pipReleaseVerdict int

const (
	// pipReleaseUnknown: makewand could not get an answer from every index.
	pipReleaseUnknown pipReleaseVerdict = iota
	// pipReleaseNone: no project, or no release matching the specifier.
	pipReleaseNone
	// pipReleaseInstallable: a matching release offers a wheel this
	// interpreter can install, or none offers anything it could use.
	pipReleaseInstallable
	// pipReleaseSdistOnly: matching releases exist, and those this
	// interpreter supports offer only source distributions.
	pipReleaseSdistOnly
)

// pipSdistSuffixes are the source distribution archives pip builds.
var pipSdistSuffixes = []string{".tar.gz", ".zip", ".tar.bz2", ".tgz", ".tar.xz", ".tar"}

// pipReleaseCheck decides, for the requirement text pip reported it found no
// distribution for, what the fetch's indexes show (see the file comment), and
// explains it for ExecResult.Note.
func (p *Project) pipReleaseCheck(ctx context.Context, env verifyExecEnvironment, sources pipFetchSources, requirement string, markerEnv pipMarkerEnv) (pipReleaseVerdict, string) {
	head, _, _ := strings.Cut(requirement, ";")
	m := pipRequirementRe.FindStringSubmatch(strings.TrimSpace(head))
	if m == nil {
		return pipReleaseUnknown, "makewand could not read the requirement " + truncateForNote(requirement) + " pip found no distribution for"
	}
	name := normalizePipName(m[1])
	rest := strings.TrimSpace(m[2])
	if strings.HasPrefix(rest, "[") {
		if end := strings.IndexByte(rest, ']'); end >= 0 {
			rest = rest[end+1:]
		}
	}
	specs, ok := parsePEP440Specifiers(rest)
	if !ok {
		return pipReleaseUnknown, "makewand could not read the version specifier of " + truncateForNote(requirement)
	}
	if sources.noIndex {
		return pipReleaseNone, "the fetch used no package index, and the local wheel directories hold no distribution for " + truncateForNote(requirement)
	}
	var files []pipIndexFile
	listed := false
	for _, index := range sources.indexes() {
		page := strings.TrimSuffix(index, "/") + "/" + name + "/"
		found, answer := pipIndexFiles(ctx, env, page, p.Path)
		switch answer {
		case pipIndexUnknown:
			return pipReleaseUnknown, "makewand could not ask the package index " + truncateForNote(redactURL(index)) + " which releases of " + name + " exist, so it cannot attribute the missing distribution " + truncateForNote(requirement) + " to fetch-only networking"
		case pipIndexListed:
			listed = true
			files = append(files, found...)
		}
	}
	if !listed {
		return pipReleaseNone, "the requirement " + truncateForNote(requirement) + " names a project the package index does not have"
	}
	exact := len(specs) == 1 && (specs[0].op == "===" || specs[0].op == "==" && !specs[0].wildcard)
	type release struct{ wheel, sdist bool }
	releases := map[string]*release{}
	for _, f := range files {
		if f.yanked && !exact {
			continue
		}
		if f.requiresPython != "" && markerEnv != nil {
			if rp, ok := parsePEP440Specifiers(f.requiresPython); ok && !pep440Contains(rp, markerEnv["python_full_version"], true) {
				continue // pip skips a file for another Python
			}
		}
		version, wheel := pipWheelVersion(f.name)
		if !wheel {
			var ok bool
			if version, ok = pipSdistVersion(f.name, name); !ok {
				continue
			}
		}
		r := releases[version]
		if r == nil {
			r = &release{}
			releases[version] = r
		}
		switch {
		case !wheel:
			r.sdist = true
		case pipWheelCompatible(f.name, markerEnv):
			r.wheel = true
		}
	}
	versions := slices.Sorted(maps.Keys(releases))
	var matching []string
	for _, v := range versions {
		if pep440Contains(specs, v, false) {
			matching = append(matching, v)
		}
	}
	if len(matching) == 0 {
		// pip takes a pre-release only when no final release matches.
		for _, v := range versions {
			if pep440Contains(specs, v, true) {
				matching = append(matching, v)
			}
		}
	}
	if len(matching) == 0 {
		return pipReleaseNone, "no release of " + name + " on the package index matches " + truncateForNote(requirement) + ", so this is the candidate's own broken dependency"
	}
	sdist := false
	for _, v := range matching {
		if releases[v].wheel {
			return pipReleaseInstallable, "the package index offers a wheel of " + name + " " + v + " for this interpreter, which matches " + truncateForNote(requirement) + ", so the wheels-only fetch does not explain why pip found none"
		}
		sdist = sdist || releases[v].sdist
	}
	if !sdist {
		return pipReleaseInstallable, "the releases of " + name + " matching " + truncateForNote(requirement) + " offer nothing this interpreter can install, so this is the candidate's own broken dependency"
	}
	return pipReleaseSdistOnly, ""
}

// pipSdistVersion returns the version in a source distribution's file name
// <name>-<version><suffix>, the name compared normalized.
func pipSdistVersion(filename, name string) (string, bool) {
	stem := ""
	lower := strings.ToLower(filename)
	for _, suffix := range pipSdistSuffixes {
		if strings.HasSuffix(lower, suffix) {
			stem = filename[:len(filename)-len(suffix)]
			break
		}
	}
	for i := 0; i < len(stem); i++ {
		if stem[i] == '-' && normalizePipName(stem[:i]) == name {
			if _, ok := parsePEP440(stem[i+1:]); ok {
				return stem[i+1:], true
			}
		}
	}
	return "", false
}

// redactURL removes credentials from a URL quoted in a message.
func redactURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	if at := strings.LastIndexByte(strings.SplitN(rest, "/", 2)[0], '@'); at >= 0 {
		rest = rest[at+1:]
	}
	return scheme + "://" + rest
}
