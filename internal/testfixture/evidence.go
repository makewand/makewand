package testfixture

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const (
	isolatedEnv = "MAKEWAND_TEST_EXECUTION_ISOLATED"
	guardEnv    = "MAKEWAND_TEST_EVIDENCE_GUARD"
)

// accountingEnv would route a test binary's model-call accounting into the
// shell's real ledger, events file or machine budget.
var accountingEnv = []string{
	"MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_EXECUTION_EVENTS_FILE", "MAKEWAND_MAX_MODEL_CALLS",
	"MAKEWAND_CALL_BUDGET_LEASE_ID", "MAKEWAND_LAYERED_BUDGET", "MAKEWAND_MACHINE_DAILY_CALLS",
	"MAKEWAND_PROJECT_DAILY_CALLS",
}

// IsolateExecutionEvidence is the first call of a TestMain whose tests can
// reach provider dispatch. It points MAKEWAND_EXECUTION_EVIDENCE_DIR at a
// fresh directory and drops accounting paths inherited from the shell, so a
// test run never writes into the user's ~/.cache/makewand/execution-evidence
// or real ledger. A child re-executed by such a test binary inherits the
// parent's settings, including what the parent test set, and is left alone.
// The returned function removes the directory.
func IsolateExecutionEvidence() (func(), error) {
	if os.Getenv(isolatedEnv) != "" {
		return func() {}, nil
	}
	dir, err := os.MkdirTemp("", "makewand-test-evidence-")
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(dir, "config"), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	for _, key := range accountingEnv {
		if err := os.Unsetenv(key); err != nil {
			return nil, err
		}
	}
	// MAKEWAND_CONFIG_DIR keeps the ledger registry and default ledger of a
	// test that names its own ledger out of the user's configuration.
	for key, value := range map[string]string{"MAKEWAND_EXECUTION_EVIDENCE_DIR": filepath.Join(dir, "evidence"),
		"MAKEWAND_CONFIG_DIR": filepath.Join(dir, "config"), isolatedEnv: dir} {
		if err := os.Setenv(key, value); err != nil {
			return nil, err
		}
	}
	return func() { _ = os.RemoveAll(dir) }, nil
}

// GuardExecutionEvidence proves that the calling test binary's TestMain
// isolates provider evidence. It runs the calling test again in a fresh copy
// of the binary that sees a private directory as the user cache and has no
// evidence directory configured, the environment of a developer shell. There
// write records evidence through the default path; the guard fails if
// anything reached the simulated user cache.
func GuardExecutionEvidence(t *testing.T, write func() error) {
	t.Helper()
	if os.Getenv(guardEnv) == "child" {
		if err := write(); err != nil {
			t.Fatal(err)
		}
		return
	}
	cache := t.TempDir()
	//nolint:gosec // G204: re-executes this test binary with a fixed test selector.
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1")
	env := []string{guardEnv + "=child", "XDG_CACHE_HOME=" + cache, "LOCALAPPDATA=" + cache}
	if runtime.GOOS == "darwin" {
		env = append(env, "HOME="+cache) // os.UserCacheDir is $HOME/Library/Caches
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case isolatedEnv, guardEnv, "MAKEWAND_EXECUTION_EVIDENCE_DIR", "XDG_CACHE_HOME", "LOCALAPPDATA":
			continue
		case "HOME":
			if runtime.GOOS == "darwin" {
				continue
			}
		}
		env = append(env, entry)
	}
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("evidence guard child: %v\n%s", err, output)
	}
	var leaked []string
	_ = filepath.WalkDir(cache, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.Contains(filepath.ToSlash(path), "/makewand/execution-evidence/") {
			leaked = append(leaked, path)
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Fatalf("a test wrote execution evidence into the user cache; isolate it in TestMain (testfixture.IsolateExecutionEvidence): %v", leaked)
	}
}
