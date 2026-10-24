//go:build linux

package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func isolateHostGoEnvCache(t *testing.T) {
	t.Helper()
	hostGoEnvMu.Lock()
	previous := hostGoEnvCache
	hostGoEnvCache = map[string]hostGoEnv{}
	hostGoEnvMu.Unlock()
	t.Cleanup(func() {
		hostGoEnvMu.Lock()
		hostGoEnvCache = previous
		hostGoEnvMu.Unlock()
	})
}

func TestPipExtraAlternativesApplyOnEitherPlatformAndStopCycles(t *testing.T) {
	project := newNeedProject(t, "pyproject.toml",
		"[project]\nname='proj'\n[project.optional-dependencies]\n"+
			"dev=[\"proj[shared] ; sys_platform == 'win32'\", \"proj[shared] ; sys_platform == 'linux'\"]\n"+
			"shared=['shareddep', \"proj[dev] ; python_version >= '3'\"]\n")
	texts, _, err := project.pipOptionalDependencyTexts(pipLocalProject{dir: ".", extras: []string{"dev"}})
	if err != nil || len(texts) != 2 {
		t.Fatalf("conditional expansion = %v, %v", texts, err)
	}
	for _, platform := range []string{"linux", "win32", "darwin"} {
		selected := 0
		for _, requirement := range texts {
			applies, known := pipRequirementApplies(requirement, pipMarkerEnv{"sys_platform": platform})
			if !known {
				t.Fatalf("could not evaluate %q", requirement)
			}
			if applies {
				selected++
			}
		}
		want := 1
		if platform == "darwin" {
			want = 0
		}
		if selected != want {
			t.Fatalf("%s selected %d dependencies, want %d", platform, selected, want)
		}
	}
}

func TestPipExcessiveExtraGraphRefusesBeforeDownload(t *testing.T) {
	var refs []string
	for i := 0; i < 1100; i++ {
		refs = append(refs, strconv.Quote("proj[shared] ; python_version == '3."+strconv.Itoa(i)+"'"))
	}
	project := newNeedProject(t, "pyproject.toml",
		"[project]\nname='proj'\n[project.optional-dependencies]\ndev=["+strings.Join(refs, ",")+"]\nshared=['dep']\n")
	_, _, cleanup, err := project.planDependencyStages(
		ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", ".[dev]"}}, project.Path)
	if cleanup != nil {
		defer cleanup()
	}
	var refusal *fetchRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.reason, "bounded static expansion") {
		t.Fatalf("excessive graph did not refuse safely before download: %v", err)
	}
}

func npmPeerArchive(t *testing.T, dir, name, metadata string) string {
	t.Helper()
	path := filepath.Join(dir, name+".tgz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	data := []byte(metadata)
	if err := archive.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(archive.Close(), compressed.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	return path
}

// Real npm resolves local metadata with networking disabled. Its ordinary
// conflict must disappear when the accepted legacy-peer-deps option reaches
// lock resolution; both peer packages must remain in the inspected lock.
func TestNpmLockResolutionRetainsPeerDependencyOptions(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm unavailable")
	}
	root := t.TempDir()
	a := npmPeerArchive(t, root, "peer-a", `{"name":"peer-a","version":"1.0.0","peerDependencies":{"peer-b":"2.x"}}`)
	b := npmPeerArchive(t, root, "peer-b", `{"name":"peer-b","version":"1.0.0"}`)
	manifest, err := json.Marshal(map[string]any{
		"name": "lock-options-fixture", "version": "1.0.0",
		"dependencies": map[string]string{"peer-a": "file:" + filepath.ToSlash(a), "peer-b": "file:" + filepath.ToSlash(b)},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "package.json"), string(manifest))
	home := t.TempDir()
	run := func(args []string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		// #nosec G204 G702 -- installed npm, fixed fixture arguments, no network or scripts.
		cmd := exec.CommandContext(ctx, npm, args...)
		cmd.Dir = root
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + home,
			"npm_config_cache=" + filepath.Join(home, "cache"),
			"npm_config_userconfig=" + filepath.Join(home, "user.conf"),
			"npm_config_globalconfig=" + filepath.Join(home, "global.conf"),
			"npm_config_offline=true", "npm_config_update_notifier=false",
		}
		return cmd.CombinedOutput()
	}
	control := []string{"install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund", "--offline"}
	if out, err := run(control); err == nil || !strings.Contains(string(out), "ERESOLVE") {
		t.Fatalf("control did not establish a peer-resolution conflict: %v %s", err, out)
	}
	args := npmResolutionArgs([]string{"install", "--legacy-peer-deps", "--omit=dev", "--no-package-lock"})
	if out, err := run(args); err != nil {
		t.Fatalf("accepted resolution options did not reach npm: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(root, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct{ Packages map[string]json.RawMessage }
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node_modules/peer-a", "node_modules/peer-b"} {
		if _, ok := lock.Packages[name]; !ok {
			t.Fatalf("resolved lock omitted %s", name)
		}
	}
}

func TestStandaloneRegistryAndIndexProbesKeepWorkspaceBoundary(t *testing.T) {
	swapVerifyIsolationVars(t)
	isolateHostGoEnvCache(t)
	workspace, outside := t.TempDir(), t.TempDir()
	marker := filepath.Join(outside, "workspace-go-ran")
	writeExecScript(t, filepath.Join(workspace, "bin", "go"),
		"printf synthetic > "+strconv.Quote(marker)+"\nprintf '{}\\n'\n")
	writeExecScript(t, filepath.Join(outside, "trusted", "go"),
		"printf '{\"GOVERSION\":\"go1.27.3\"}\\n'\n")
	t.Setenv("PATH", strings.Join([]string{filepath.Join(workspace, "bin"), filepath.Join(outside, "trusted"), "/usr/bin", "/bin"}, ":"))
	verifyGetenv = os.Getenv
	sandboxGoEnvProbe = probeHostGoEnv
	previousLookup := userNetLookPath
	t.Cleanup(func() { userNetLookPath = previousLookup })
	userNetLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	// No network or candidate process can start. The defect happens in host
	// layout preparation, before this deliberately unavailable sandbox.
	env := verifyExecEnvironment{mode: verifyExecIsolated, bwrapPath: "/bin/false"}
	project := &Project{Path: workspace}
	for _, probe := range []struct {
		name string
		run  func()
	}{
		{"registry", func() { _ = checkRegistryReachable(context.Background(), env, "npm", workspace) }},
		{"pip index", func() {
			_, _ = project.pipReleaseCheck(context.Background(), env, pipFetchSources{index: "https://example.invalid/simple"}, "example>=1", pipMarkerEnv{})
		}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			hostGoEnvMu.Lock()
			hostGoEnvCache = map[string]hostGoEnv{}
			hostGoEnvMu.Unlock()
			_ = os.Remove(marker)
			probe.run()
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("standalone probe executed the workspace Go: %v", err)
			}
		})
	}
}

func TestHostGoRefusesUnresolvedEnvShebangs(t *testing.T) {
	swapVerifyIsolationVars(t)
	isolateHostGoEnvCache(t)
	workspace, outside := t.TempDir(), t.TempDir()
	marker := filepath.Join(outside, "workspace-interpreter-ran")
	writeExecScript(t, filepath.Join(workspace, "bin", "bash"),
		"printf synthetic > "+strconv.Quote(marker)+"\nprintf '{}\\n'\n")
	writeExecScript(t, filepath.Join(workspace, "bin", "env"),
		"printf synthetic > "+strconv.Quote(marker)+"\nprintf '{}\\n'\n")
	trustedDir, suspectDir := filepath.Join(outside, "trusted"), filepath.Join(outside, "suspect")
	writeExecScript(t, filepath.Join(trustedDir, "go"), "printf '{\"GOVERSION\":\"go1.27.3\"}\\n'\n")
	entry := filepath.Join(suspectDir, "go")
	writeExecScript(t, entry, "")
	pathEnv := strings.Join([]string{suspectDir, trustedDir, "/usr/bin", "/bin"}, ":")
	t.Setenv("PATH", pathEnv)
	sandboxGoEnvProbe = probeHostGoEnv
	for _, shebang := range []string{
		"#!/usr/bin/env -S PATH=" + filepath.Join(workspace, "bin") + " bash",
		"#!/usr/bin/env -S",
		"#!/usr/bin/env -u PATH bash",
		"#!" + filepath.Join(workspace, "bin", "env") + " bash",
	} {
		t.Run(shebang, func(t *testing.T) {
			hostGoEnvMu.Lock()
			hostGoEnvCache = map[string]hostGoEnv{}
			hostGoEnvMu.Unlock()
			_ = os.Remove(marker)
			if err := os.WriteFile(entry, []byte(shebang+"\nprintf '{}\\n'\n"), 0o700); err != nil { //nolint:gosec // executable test fixture
				t.Fatal(err)
			}
			if trustedFetchExecutable(entry, workspace, pathEnv) {
				t.Error("unsupported or workspace-controlled interpreter was accepted")
			}
			_, _, version := resolveSandboxToolchains(outside, workspace, pathEnv, func(string) string { return "" }, true)
			if version != "go1.27.3" {
				t.Errorf("trusted fallback version = %q", version)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace interpreter executed: %v", err)
			}
		})
	}
}
