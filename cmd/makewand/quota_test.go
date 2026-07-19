package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestQuotaUntrustedSkipsLocalCLISource verifies that `makewand quota
// --repo-trust=untrusted` never execs a local-CLI quota source: the fake `agy`
// binary's sentinel is not written. The trusted run proves the fixture is
// otherwise reachable (it is exec'd and writes the sentinel), so the untrusted
// skip is meaningful rather than a false pass.
func TestQuotaUntrustedSkipsLocalCLISource(t *testing.T) {
	binDir := t.TempDir()

	// Fake `agy` on PATH: the agy quota source execs `agy models`. Create the
	// sentinel via a shell builtin + redirection because the exec inherits the
	// restricted test PATH (no external `touch` available).
	sentinel := filepath.Join(binDir, "agy_models_ran")
	script := filepath.Join(binDir, "agy")
	body := "#!/bin/sh\nprintf 'ran\\n' > \"" + sentinel + "\"\nprintf 'model-a\\nmodel-b\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture must be executable.
		t.Fatalf("WriteFile(script): %v", err)
	}

	run := func(t *testing.T, trust string) {
		t.Helper()
		// Fresh HOME so the shared quota disk cache is empty (a cached snapshot is
		// read-through and would bypass the sources entirely).
		t.Setenv("HOME", t.TempDir())
		t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
		t.Setenv("PATH", binDir)
		_ = os.Remove(sentinel)

		cmd := newRootCmd()
		cmd.SetArgs([]string{"quota", "--repo-trust=" + trust})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("quota --repo-trust=%s error: %v", trust, err)
		}
	}

	t.Run("untrusted does not exec local CLI", func(t *testing.T) {
		run(t, "untrusted")
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatalf("local-CLI quota source was executed in untrusted mode (sentinel %q exists)", sentinel)
		}
	})

	t.Run("trusted execs local CLI", func(t *testing.T) {
		run(t, "trusted")
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("trusted quota did not exec the local-CLI source (sentinel %q missing): %v", sentinel, err)
		}
	})
}
