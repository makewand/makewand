package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
