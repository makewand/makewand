//go:build linux

package engine

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GEC-1021-1 round 3 (review of the round-2 fix): the cargo config files of
// the sandbox CARGO_HOME are not cache. makewand never writes them, a loopback
// step running project code can, and a credential provider they name still
// runs under cargoFetchEnv (verified on cargo 1.93). Since round 7 the
// networked fetch's CARGO_HOME is the shared cargo cache outside the
// workspace, which only the host user and the fetches write, and the fetch
// sees no part of the workspace, the sandbox HOME included.
func TestNetworkedFetchMasksSandboxCargoConfig_GEC1021R3(t *testing.T) {
	project := newNeedProject(t,
		"Cargo.toml", "[package]\nname = \"x\"\nversion = \"0.1.0\"\n",
		filepath.Join(sandboxHomeRelPath, ".cargo", "config.toml"), "[registry]\nglobal-credential-providers = [\"/evil\"]\n",
		filepath.Join(sandboxHomeRelPath, ".cargo", "config"), "[net]\nretry = 2\n")
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"build"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	home := ""
	for _, setting := range fetch.env {
		if v, ok := strings.CutPrefix(setting, "CARGO_HOME="); ok {
			home = v
		}
	}
	if want := filepath.Join(fetch.stage.area.cache, fetchCacheCargo); home != want {
		t.Fatalf("fetch CARGO_HOME = %q, want the shared cargo cache %q", home, want)
	}
	if overlapping(home, project.Path) {
		t.Fatalf("fetch CARGO_HOME %s lies in the workspace", home)
	}
	spec := fetchSpec{workspace: project.Path, dir: fetch.stage.dir.path, cache: fetch.stage.area.cache, pathEnv: fetchPathEnv(project.Path), env: fetch.env}
	_, args, err := wrapFetchCommand("/usr/bin/bwrap", spec, "cargo", fetch.Args)
	if err != nil {
		t.Fatal(err)
	}
	binds, _ := boundPaths(args)
	sandboxHome := filepath.Join(project.Path, filepath.FromSlash(sandboxHomeRelPath))
	if slices.ContainsFunc(binds, func(p string) bool { return pathWithin(sandboxHome, p) }) {
		t.Fatalf("the fetch binds the sandbox HOME: %q", args)
	}
}
