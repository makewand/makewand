//go:build linux

package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pipGuardTools(t *testing.T, body string) (string, string) {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("pip execution guard supports Linux amd64/arm64")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	if out, err := exec.Command(python, "-I", "-c", "import pip").CombinedOutput(); err != nil {
		t.Skipf("isolated Python has no pip: %v %s", err, out)
	}
	tools := t.TempDir()
	entry := filepath.Join(tools, "pip")
	if err := os.WriteFile(entry, []byte("#!"+python+"\n"+body), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	swapVerifyIsolationVars(t)
	path := tools + ":" + os.Getenv("PATH")
	verifyGetenv = func(key string) string {
		if key == "PATH" {
			return path
		}
		return os.Getenv(key)
	}
	return python, entry
}

func runPipGuard(t *testing.T, workspace string, args ...string) ([]byte, string, error) {
	t.Helper()
	command, guarded, tag, refusal := guardedPipFetch(workspace, "pip", args)
	if refusal != "" {
		t.Fatal(refusal)
	}
	cmd := exec.CommandContext(t.Context(), command, guarded...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE="+os.DevNull, "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_CACHE_DIR=1", "PIP_NO_INPUT=1")
	out, err := cmd.CombinedOutput()
	return out, tag, err
}

func TestPipFetchKernelGuardDeniesExecWithoutRelyingOnAuditHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	// ctypes calls libc directly and emits no Python subprocess audit event.
	// Without the kernel filter this replaces the interpreter with a shell.
	pipGuardTools(t, `import ctypes, errno
libc = ctypes.CDLL(None, use_errno=True)
argv = (ctypes.c_char_p * 4)(b"sh", b"-c", `+strconv.Quote("printf ran > "+marker)+`.encode(), None)
env = (ctypes.c_char_p * 1)(None)
assert libc.execve(b"/bin/sh", argv, env) == -1
assert ctypes.get_errno() == errno.EPERM
print("kernel denied exec")
`)
	out, _, err := runPipGuard(t, t.TempDir())
	if err != nil || !strings.Contains(string(out), "kernel denied exec") {
		t.Fatalf("raw exec boundary: %v %s", err, out)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("raw exec ran a child: %v", err)
	}
}

func TestPipFetchGuardRefusesSubprocessAndInvalidFilter(t *testing.T) {
	pipGuardTools(t, "import subprocess\nsubprocess.run(['/bin/true'], check=True)\n")
	workspace := t.TempDir()
	out, tag, err := runPipGuard(t, workspace)
	if err == nil || !strings.Contains(string(out), tag) {
		t.Fatalf("subprocess did not produce the trusted refusal: %v %s", err, out)
	}
	command, args, tag, refusal := guardedPipFetch(workspace, "pip", nil)
	if refusal != "" {
		t.Fatal(refusal)
	}
	args[3] = "not-a-filter"
	if out, err := exec.CommandContext(t.Context(), command, args...).CombinedOutput(); err == nil || !strings.Contains(string(out), tag) {
		t.Fatalf("invalid filter must fail closed before pip: %v %s", err, out)
	}
}

func TestPipFetchedWheelSourceReferenceCannotExecuteBackend(t *testing.T) {
	python, _ := pipGuardTools(t, "from pip._internal.cli.main import main\nraise SystemExit(main())\n")
	root, workspace := t.TempDir(), t.TempDir()
	marker := filepath.Join(root, "backend-ran")
	archive := filepath.Join(root, "evil-1.0.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for _, item := range [][2]string{
		{"pyproject.toml", "[build-system]\nrequires=[]\nbuild-backend='backend'\nbackend-path=['.']\n"},
		{"backend.py", "from pathlib import Path\nPath(" + strconv.Quote(marker) + ").write_text('backend executed')\nraise RuntimeError('fixture backend executed')\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: "evil-1.0/" + item[0], Mode: 0o600, Size: int64(len(item[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(item[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), gz.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(root)))
	defer server.Close()
	index := filepath.Join(root, "index")
	writeInstallableWheel(t, index, "app", "Requires-Dist: evil @ "+server.URL+"/evil-1.0.tar.gz\n")
	args := []string{"download", "--only-binary=:all:", "--no-build-isolation", "--no-index", "--find-links", index, "--dest", filepath.Join(root, "control"), "app"}
	// Establish that this pip really reaches the malicious backend under the
	// old binary-only policy; all content comes from our loopback server.
	control := exec.CommandContext(t.Context(), python, append([]string{"-I", "-m", "pip"}, args...)...)
	control.Env = append(os.Environ(), "PIP_CONFIG_FILE="+os.DevNull, "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_CACHE_DIR=1")
	if out, err := control.CombinedOutput(); err == nil {
		t.Fatalf("fixture backend unexpectedly succeeded: %s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control did not reproduce source backend execution: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	out, tag, err := runPipGuard(t, workspace, args...)
	if err == nil || !strings.Contains(string(out), tag) {
		t.Fatalf("guard did not refuse the transitive source build: %v %s", err, out)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("downloaded backend ran with the guard: %v", err)
	}
	// Normal transitive wheel resolution still works; --no-deps would break it.
	writeInstallableWheel(t, index, "plain", "Requires-Dist: child\n")
	writeInstallableWheel(t, index, "child", "")
	dest := filepath.Join(root, "wheels")
	out, _, err = runPipGuard(t, workspace, "download", "--only-binary=:all:", "--no-index", "--find-links", index, "--dest", dest, "plain")
	if err != nil {
		t.Fatalf("static wheel dependencies stopped working: %v %s", err, out)
	}
	for _, name := range []string{"plain", "child"} {
		if _, err := os.Stat(filepath.Join(dest, name+"-1.0-py3-none-any.whl")); err != nil {
			t.Fatalf("transitive wheel %s missing: %v", name, err)
		}
	}
}

func TestHostGoProbeNeverExecutesWorkspaceTools(t *testing.T) {
	swapVerifyIsolationVars(t)
	workspace, outside := t.TempDir(), t.TempDir()
	marker := filepath.Join(outside, "workspace-go-ran")
	writeExecScript(t, filepath.Join(workspace, "bin", "go"), "printf '%s' \"$MAKEWAND_TEST_HOST_CANARY\" > "+strconv.Quote(marker)+"\nprintf '{}\\n'\n")
	writeExecScript(t, filepath.Join(outside, "good", "go"), "printf '{\"GOVERSION\":\"go1.27.3\"}\\n'\n")
	alias := filepath.Join(outside, "alias")
	if err := os.Symlink(filepath.Join(workspace, "bin"), alias); err != nil {
		t.Fatal(err)
	}
	interpreterDir := filepath.Join(outside, "interpreter")
	writeExecScript(t, filepath.Join(interpreterDir, "go"), "")
	if err := os.WriteFile(filepath.Join(interpreterDir, "go"), []byte("#!"+filepath.Join(workspace, "bin", "go")+"\n"), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_TEST_HOST_CANARY", "synthetic-private-value")
	sandboxGoEnvProbe = probeHostGoEnv
	path := strings.Join([]string{filepath.Join(workspace, "bin"), alias, interpreterDir, filepath.Join(outside, "good"), "/usr/bin", "/bin"}, ":")
	_, _, version := resolveSandboxToolchains(outside, workspace, path, func(string) string { return "" }, true)
	if version != "go1.27.3" {
		t.Fatalf("trusted Go was not selected: %q", version)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace go ran on the host: %v", err)
	}
}

// Match a Python pip entry point so fixtures exercise the real execution guard.
func writePipFixture(t *testing.T, path, body string) {
	t.Helper()
	for _, python := range []string{"/usr/local/bin/python3", "/usr/bin/python3"} {
		if err := exec.Command(python, "-I", "-c", "import pip").Run(); err != nil {
			continue
		}
		if err := os.WriteFile(path, []byte("#!"+python+"\nimport os, sys\nfrom pathlib import Path\n"+body), 0o700); err != nil { //nolint:gosec // executable test fixture
			t.Fatal(err)
		}
		return
	}
	t.Skip("Python with pip unavailable for guarded fetch fixture")
}

func TestNpmFetchCannotReenableLifecycleScripts(t *testing.T) {
	for _, flag := range []string{"--ignore-scripts=false", "--ignore-scripts=0", "--ignore-scripts=", "--no-ignore-scripts"} {
		if npmNetworkedOption(flag) {
			t.Fatalf("networked fetch accepts %q", flag)
		}
		args := npmInstallArgs([]string{"ci", flag}, true)
		if slices.Contains(args, flag) || !slices.Contains(args, "--ignore-scripts") {
			t.Fatalf("unsafe lifecycle flag survived normalization: %q", args)
		}
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm unavailable for the live lifecycle check")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"guard-test","version":"1.0.0","scripts":{"preinstall":"node -e \"require('fs').writeFileSync('lifecycle-ran','bad')\""}}`)
	writeFile(t, filepath.Join(dir, "package-lock.json"), `{"name":"guard-test","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"guard-test","version":"1.0.0"}}}`)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, npm, npmInstallArgs([]string{"ci", "--ignore-scripts=false"}, true)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "npm_config_offline=true", "npm_config_update_notifier=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("safe npm ci failed: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "lifecycle-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("npm lifecycle script ran: %v", err)
	}
}
