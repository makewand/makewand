package router

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// cliTestPrivateHomeBase holds the private provider homes created by tests so
// mount-plan tests, whose commands never run, do not leave run directories
// in the developer's cache directory. It is created on first use: helper
// processes that re-execute the test binary exit before TestMain cleans up.
var (
	cliTestPrivateHomeBase     string
	cliTestPrivateHomeBaseOnce sync.Once
)

func cliTestPrivateHomeBaseDir() string {
	cliTestPrivateHomeBaseOnce.Do(func() {
		if base, err := os.MkdirTemp("", "makewand-router-cli-homes-"); err == nil {
			cliTestPrivateHomeBase = base
		}
	})
	return cliTestPrivateHomeBase
}

func TestMain(m *testing.M) {
	cliTestExtraBinds = cliTestFixtureBinds
	cliTestEnvAllowed = cliTestFixtureEnvAllowed
	cliPrivateHomeBaseDir = cliTestPrivateHomeBaseDir
	// Without bubblewrap (always on Windows) every provider launch needs an
	// acknowledged host-exec authorization, exactly as in production. Process
	// tests then run their fixtures through that real config-ack path.
	// MAKEWAND_TEST_SIMULATE_NO_BWRAP=1 reproduces that mode on Linux.
	// A bwrap binary that cannot mount /proc under --unshare-pid cannot run
	// the sandbox these tests launch, so it is treated as missing too.
	// MAKEWAND_TEST_REQUIRE_BWRAP=1 keeps the real binary and lets live tests
	// fail instead of skipping.
	ackDir := ""
	bwrapUnusable := runtime.GOOS == "linux" &&
		os.Getenv("MAKEWAND_TEST_SIMULATE_NO_BWRAP") != "1" &&
		os.Getenv("MAKEWAND_TEST_REQUIRE_BWRAP") != "1" &&
		!cliLiveBwrapWorks()
	if runtime.GOOS == "windows" || os.Getenv("MAKEWAND_TEST_SIMULATE_NO_BWRAP") == "1" || bwrapUnusable {
		if runtime.GOOS != "windows" {
			cliBwrapLookup = func(string) (string, error) { return "", exec.ErrNotFound }
		}
		var err error
		if ackDir, err = cliTestWriteHostExecAck(); err != nil {
			fmt.Fprintln(os.Stderr, "host-exec ack fixture:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if cliTestPrivateHomeBase != "" {
		_ = os.RemoveAll(cliTestPrivateHomeBase)
	}
	if ackDir != "" {
		_ = os.RemoveAll(ackDir)
	}
	os.Exit(code)
}

// cliTestWriteHostExecAck writes a valid host-exec acknowledgement into a
// temporary MAKEWAND_CONFIG_DIR and enables MAKEWAND_UNSAFE_HOST_EXEC. Tests
// asserting refusal without an acknowledgement point MAKEWAND_CONFIG_DIR at
// an empty directory (cliTestWithoutHostExecAck).
func cliTestWriteHostExecAck() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "makewand-router-ack-")
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{
		"unsafe_host_exec_ack_version": 1,
		"unsafe_host_exec_ack_host":    host,
		"unsafe_host_exec_ack_at":      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return dir, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		return dir, err
	}
	if err := os.Setenv("MAKEWAND_CONFIG_DIR", dir); err != nil {
		return dir, err
	}
	return dir, os.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
}

// cliTestWithoutHostExecAck removes any process-wide acknowledgement for the
// rest of the test.
func cliTestWithoutHostExecAck(t *testing.T) {
	t.Helper()
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
}

// cliTestFixtureBinds exposes the go test fixtures a sandboxed helper process
// needs, which the sandbox would hide: it replaces /tmp, /var/tmp and /run
// with empty tmpfs mounts and masks /home, /mnt, /root, /media and /srv, any
// of which may hold the temporary directory go test works in (os.TempDir, so
// TMPDIR). Exposed are, below that temporary directory, the build directory
// holding the test binary (go-build*, read-only) and the t.TempDir roots
// (Test*, writable) holding the fixture copies of the binary and the markers
// and state files the helpers write; and the directory of the test binary
// itself wherever it is (GOTMPDIR, go test -c -o), read-only. Production
// builds never install this hook.
func cliTestFixtureBinds(targetBin string, env []string) []string {
	var args, roots []string
	covered := func(path string) bool {
		for _, root := range roots {
			if path == root || cliPathWithin(root, path) {
				return true
			}
		}
		return false
	}
	add := func(flag, root string) {
		if covered(root) {
			return
		}
		if _, err := os.Stat(root); err != nil {
			return
		}
		roots = append(roots, root)
		args = append(args, flag, root, root)
	}
	temps := cliTestTempDirs()
	bind := func(targetPath string) {
		if targetPath == "" || !filepath.IsAbs(targetPath) {
			return
		}
		targetPath = filepath.Clean(targetPath)
		for _, temp := range temps {
			if targetPath == temp || !cliPathWithin(temp, targetPath) {
				continue
			}
			rel, err := filepath.Rel(temp, targetPath)
			if err != nil {
				continue
			}
			top := strings.Split(rel, string(os.PathSeparator))[0]
			root := filepath.Join(temp, top)
			switch {
			case strings.HasPrefix(top, "go-build"):
				add("--ro-bind", root)
			case strings.HasPrefix(top, "Test"):
				add("--bind", root)
			}
			return
		}
	}
	bind(targetBin)
	if exe, err := os.Executable(); err == nil && cliTestSameFile(exe, targetBin) {
		// The test binary itself runs as the helper: its build directory, or
		// its own directory wherever go test built it.
		bind(exe)
		if dir := filepath.Dir(exe); dir != "/" && !covered(dir) && !covered(filepath.Dir(filepath.Clean(targetBin))) {
			add("--ro-bind", dir)
		}
	}
	for _, entry := range env {
		if marker, ok := strings.CutPrefix(entry, "MAKEWAND_CLI_PROCESS_MARKER="); ok {
			bind(marker)
		}
	}
	return args
}

// cliTestSameFile reports whether a and b name the same existing file.
func cliTestSameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// cliTestStartTempDir is the temporary directory go test started the test
// binary with: it holds the build directory and every t.TempDir root, even
// while a test points TMPDIR somewhere else.
var cliTestStartTempDir = os.TempDir()

// cliTestTempDirs returns the temporary directories fixtures live in: the
// one the test binary started with and the current os.TempDir, each as given
// and, when it differs, with its symbolic links resolved (paths from
// t.TempDir carry the first form, os.Executable may return the second).
func cliTestTempDirs() []string {
	var dirs []string
	for _, temp := range []string{os.Getenv("GOTMPDIR"), cliTestStartTempDir, os.TempDir()} {
		temp = filepath.Clean(temp)
		if !filepath.IsAbs(temp) || temp == "/" {
			continue
		}
		forms := []string{temp}
		if resolved, err := filepath.EvalSymlinks(temp); err == nil && resolved != temp && resolved != "/" {
			forms = append(forms, resolved)
		}
		for _, form := range forms {
			if !slices.Contains(dirs, form) {
				dirs = append(dirs, form)
			}
		}
	}
	// A test may set TMPDIR inside GOTMPDIR (or the reverse). Match the
	// most specific namespace before a broader parent hides its layout.
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
	return dirs
}

// cliTestFixtureEnvAllowed passes helper-process control variables used by
// router tests that re-execute the test binary as a fake provider CLI.
func cliTestFixtureEnvAllowed(key string) bool {
	for _, prefix := range []string{"MAKEWAND_CLI_PROCESS_", "MAKEWAND_CLAUDE_ERROR_TEST_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// withoutCLITestHooks runs a test against the production configuration.
func withoutCLITestHooks(t *testing.T) {
	t.Helper()
	oldBinds, oldEnv := cliTestExtraBinds, cliTestEnvAllowed
	cliTestExtraBinds, cliTestEnvAllowed = nil, nil
	t.Cleanup(func() { cliTestExtraBinds, cliTestEnvAllowed = oldBinds, oldEnv })
}

// cliPrivateHomeSource returns the source of the writable bind mounted at
// dest (the sandbox HOME or CODEX_HOME), failing unless it is a per-run
// private directory rather than the host path itself.
func cliPrivateHomeSource(t *testing.T, args []string, dest string) string {
	t.Helper()
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+2] == dest {
			src := args[i+1]
			if src == dest || cliTestPrivateHomeBase == "" || !cliPathWithin(cliTestPrivateHomeBase, src) {
				t.Fatalf("%s is bound writable from %s, want a private per-run directory: %v", dest, src, args)
			}
			return src
		}
	}
	t.Fatalf("no private writable bind at %s: %v", dest, args)
	return ""
}

func TestFixtureBindsNestedGoTempDirWritable(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "gotmp")
	fixture := filepath.Join(nested, "TestNestedFixture123")
	if err := os.MkdirAll(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTMPDIR", nested)
	args := cliTestFixtureBinds(filepath.Join(fixture, "fake-cli"), nil)
	if !strings.Contains(strings.Join(args, " "), "--bind "+fixture+" "+fixture) {
		t.Fatalf("Go fixture has no writable bind: %v", args)
	}
}
