package router

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
	code := m.Run()
	if cliTestPrivateHomeBase != "" {
		_ = os.RemoveAll(cliTestPrivateHomeBase)
	}
	os.Exit(code)
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
