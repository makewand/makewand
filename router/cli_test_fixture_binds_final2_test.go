package router

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Final round 2 (tmpdir-fixtures): the sandbox binds of the test fixtures
// follow the temporary directory go test works in (TMPDIR), not /tmp: below
// it the build directory is bound read-only and the t.TempDir root holding a
// fixture writable; anything else is left hidden.
func TestCLITestFixtureBindsFollowTheTempDir_Final2(t *testing.T) {
	base := t.TempDir()
	old := cliTestStartTempDir
	cliTestStartTempDir = base
	t.Cleanup(func() { cliTestStartTempDir = old })
	build := filepath.Join(base, "go-build123", "b001")
	root := filepath.Join(base, "TestSomething456")
	other := filepath.Join(base, "other")
	for _, dir := range []string{build, filepath.Join(root, "001"), filepath.Join(root, "002"), other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name      string
		targetBin string
		env       []string
		want      []string
	}{
		{"test binary and a marker", filepath.Join(build, "router.test"), []string{"MAKEWAND_CLI_PROCESS_MARKER=" + filepath.Join(root, "002", "marker")},
			[]string{"--ro-bind", filepath.Join(base, "go-build123"), filepath.Join(base, "go-build123"), "--bind", root, root}},
		{"fixture copy of the binary", filepath.Join(root, "001", "cli"), nil, []string{"--bind", root, root}},
		{"one root bound once", filepath.Join(root, "001", "cli"), []string{"MAKEWAND_CLI_PROCESS_MARKER=" + filepath.Join(root, "002", "marker")}, []string{"--bind", root, root}},
		{"other directories stay hidden", filepath.Join(other, "cli"), nil, nil},
	} {
		if got := cliTestFixtureBinds(tc.targetBin, tc.env); !slices.Equal(got, tc.want) {
			t.Errorf("%s: binds = %q, want %q", tc.name, got, tc.want)
		}
	}
}
