package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/makewand/makewand/internal/testfixture"
)

// TestQuotaUntrustedSkipsLocalCLISource verifies that `makewand quota
// --repo-trust=untrusted` never execs a local-CLI quota source: the fake `agy`
// binary's sentinel is not written. The trusted run proves the fixture is
// otherwise reachable (it is exec'd and writes the sentinel), so the untrusted
// skip is meaningful rather than a false pass.
func TestQuotaUntrustedSkipsLocalCLISource(t *testing.T) {
	binDir := t.TempDir()

	// A native fixture proves the trusted source is reachable on every OS.
	sentinel := filepath.Join(binDir, "agy_models_ran")
	testfixture.WriteCLIAt(t, binDir, "agy", testfixture.Spec{Stdout: "model-a\nmodel-b\n", Sentinel: sentinel})

	run := func(t *testing.T, trust string) {
		t.Helper()
		// Fresh HOME so the shared quota disk cache is empty (a cached snapshot is
		// read-through and would bypass the sources entirely).
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
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

func TestQuotaDisabledProviderSkipsCLIAndCachedData(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "agy-executed")
	testfixture.WriteCLIAt(t, dir, "agy", testfixture.Spec{Stdout: "model-a\n", Sentinel: sentinel})
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAKEWAND_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"enabled_providers":{"google":false,"claude":false,"codex":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	command := newRootCmd()
	command.SetArgs([]string{"quota", "--json"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("quota probed disabled provider", err)
	}
}
