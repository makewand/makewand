//go:build linux

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// An outside Go wrapper can still invoke a workspace interpreter through env.
// Checking its shebang against one PATH but executing with another is unsafe.
func TestHostGoProbeWrappersCannotSelectWorkspaceInterpreter(t *testing.T) {
	swapVerifyIsolationVars(t)
	workspace, outside := t.TempDir(), t.TempDir()
	marker := filepath.Join(outside, "workspace-interpreter-ran")
	writeExecScript(t, filepath.Join(workspace, "bin", "bash"),
		"printf '%s' \"$MAKEWAND_TEST_HOST_CANARY\" > "+strconv.Quote(marker)+"\nprintf '{}\\n'\n")
	goBin := filepath.Join(outside, "trusted", "go")
	writeExecScript(t, goBin, "")
	wrapper := `#!/usr/bin/env bash
if [ "$1" = env ]; then
  printf '%s\n' '{"GOVERSION":"go1.27.3","GOPROXY":"off","GOSUMDB":"off"}'
else
  printf '%s\n' '{"Module":{"Path":"example.com/app"},"Replace":[{"Old":{"Path":"example.com/dep"},"New":{"Path":"./local"}}]}'
fi
`
	if err := os.WriteFile(goBin, []byte(wrapper), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	pathEnv := strings.Join([]string{filepath.Join(workspace, "bin"), filepath.Dir(goBin), "/usr/bin", "/bin"}, ":")
	t.Setenv("PATH", pathEnv)
	t.Setenv("MAKEWAND_TEST_HOST_CANARY", "synthetic-private-value")
	verifyGetenv = os.Getenv
	sandboxGoEnvProbe = probeHostGoEnv
	hostGoEnvMu.Lock()
	previousCache := hostGoEnvCache
	hostGoEnvMu.Unlock()
	t.Cleanup(func() {
		hostGoEnvMu.Lock()
		hostGoEnvCache = previousCache
		hostGoEnvMu.Unlock()
	})
	project := &Project{Path: workspace}
	gomod := "module example.com/app\nreplace example.com/dep => ./local\n"
	writeFile(t, filepath.Join(workspace, "go.mod"), gomod)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeWorkspace, err := filepath.Rel(cwd, workspace)
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name string
		run  func(*testing.T)
	}{
		{"toolchain layout", func(t *testing.T) {
			_, _, version := resolveSandboxToolchains(outside, workspace, fetchPathEnv(workspace), func(string) string { return "" }, true)
			if version != "go1.27.3" {
				t.Errorf("trusted wrapper version = %q", version)
			}
		}},
		{"local replacement parser", func(t *testing.T) {
			dirs, err := project.goLocalReplacements(workspace, ".", []byte(gomod))
			if err != nil || len(dirs) != 1 || dirs[0] != "local" {
				t.Errorf("local replacements = %v, %v", dirs, err)
			}
		}},
		{"offline need parser", func(*testing.T) {
			_ = project.goRegistryNeed(ExecPlan{Command: "go", Args: []string{"mod", "tidy"}})
		}},
		{"registry endpoints", func(*testing.T) { _ = goRegistryEndpoints(workspace) }},
		{"relative workspace endpoints", func(*testing.T) { _ = goRegistryEndpoints(relativeWorkspace) }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			hostGoEnvMu.Lock()
			hostGoEnvCache = map[string]hostGoEnv{}
			hostGoEnvMu.Unlock()
			_ = os.Remove(marker)
			check.run(t)
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("host probe executed the workspace interpreter: %v", err)
			}
		})
	}
}

func TestHostGoEnvCacheIncludesExecutionPath(t *testing.T) {
	root := t.TempDir()
	goBin := filepath.Join(root, "go")
	if err := os.WriteFile(goBin, []byte("#!/usr/bin/env makewand-env-helper\n"), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	for _, version := range []string{"go1.27.1", "go1.27.2", "go1.27.1"} {
		dir := filepath.Join(root, version)
		writeExecScript(t, filepath.Join(dir, "makewand-env-helper"),
			"printf '%s\\n' '{\"GOVERSION\":\""+version+"\"}'\n")
		env, err := probeHostGoEnv(goBin, dir+":/usr/bin:/bin")
		if err != nil || env.GOVERSION != version {
			t.Fatalf("probe with %s PATH returned version %q: %v", version, env.GOVERSION, err)
		}
	}
}
