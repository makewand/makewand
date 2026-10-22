package engine

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps the host-owned dependency fetch area (depFetchBaseDir) of
// every test in a temporary directory of its own, never the user's cache, and
// the host's pip configuration out of the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "makewand-dep-fetch-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	depFetchBaseDir = func() (string, error) { return dir, nil }
	pipHostConfigFiles = nil // the host's pip configuration is not the tests'
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// disableGoTelemetryAt turns go telemetry off in the HOME dir of a test.
func disableGoTelemetryAt(t *testing.T, dir string) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := disableGoTelemetry(root); err != nil {
		t.Fatal(err)
	}
}

// isolateFetchArea gives a test a dependency fetch area of its own, so caches
// another test's fetches filled cannot change what it sees.
func isolateFetchArea(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := depFetchBaseDir
	depFetchBaseDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { depFetchBaseDir = old })
}
