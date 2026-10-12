package router

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	ackDir := ""
	if runtime.GOOS == "windows" || os.Getenv("MAKEWAND_TEST_SIMULATE_NO_BWRAP") == "1" {
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

// cliTestFixtureBinds exposes go test fixtures under /tmp, which the sandbox
// replaces with a tmpfs: the test binary (go-build*, read-only) and the
// t.TempDir roots (Test*, writable) holding helper markers. Production builds
// never install this hook.
func cliTestFixtureBinds(targetBin string, env []string) []string {
	var args []string
	bind := func(targetPath string) {
		if !cliPathWithin("/tmp", targetPath) || targetPath == "/tmp" {
			return
		}
		rel, err := filepath.Rel("/tmp", targetPath)
		if err != nil {
			return
		}
		top := strings.Split(rel, string(os.PathSeparator))[0]
		root := filepath.Join("/tmp", top)
		if _, err := os.Stat(root); err != nil {
			return
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i+1] == root {
				return
			}
		}
		switch {
		case strings.HasPrefix(top, "go-build"):
			args = append(args, "--ro-bind", root, root)
		case strings.HasPrefix(top, "Test"):
			args = append(args, "--bind", root, root)
		}
	}
	bind(targetBin)
	for _, entry := range env {
		if marker, ok := strings.CutPrefix(entry, "MAKEWAND_CLI_PROCESS_MARKER="); ok {
			bind(marker)
		}
	}
	return args
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
