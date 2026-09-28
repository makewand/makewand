package config

import (
	"os"
	"os/exec"
	"testing"
)

// TestMain makes the package hermetic. The developer or CI environment must
// not leak into config tests: MAKEWAND_CONFIG_DIR would redirect ConfigDir
// away from each test's temporary HOME, and the real PATH would let Load's
// CLI detection exec installed provider CLIs (claude/codex/gemini/agy
// --version). Tests that need fake CLIs put their own directory on PATH.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("MAKEWAND_CONFIG_DIR")
	emptyPath, err := os.MkdirTemp("", "makewand-config-test-path-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("PATH", emptyPath)
	code := m.Run()
	_ = os.RemoveAll(emptyPath)
	os.Exit(code)
}

// useTempHome points HOME at home and clears MAKEWAND_CONFIG_DIR for one test,
// so ConfigDir resolves to home/.config/makewand whatever the caller's
// environment sets.
func useTempHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("MAKEWAND_CONFIG_DIR", "")
}

// TestConfigTestsAreHermetic guards the TestMain isolation: no inherited
// config-dir override, and no provider CLI reachable for Load to exec.
func TestConfigTestsAreHermetic(t *testing.T) {
	if dir := os.Getenv("MAKEWAND_CONFIG_DIR"); dir != "" {
		t.Fatalf("MAKEWAND_CONFIG_DIR leaked into config tests: %q", dir)
	}
	for _, bin := range []string{"claude", "codex", "gemini", "agy"} {
		if path, err := exec.LookPath(bin); err == nil {
			t.Fatalf("provider CLI %s reachable on test PATH (%s); Load would exec it", bin, path)
		}
	}
}

// TestLoad_ParseErrorIgnoresInheritedConfigDir reproduces the original
// failure: an externally set MAKEWAND_CONFIG_DIR made the HOME-based parse
// error test read another directory and see no parse error.
func TestLoad_ParseErrorIgnoresInheritedConfigDir(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir()) // simulate the caller's isolation env
	home := t.TempDir()
	useTempHome(t, home)
	dir, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/config.json", []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want parse error from the per-test HOME config")
	}
}
