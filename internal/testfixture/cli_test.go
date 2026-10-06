package testfixture

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureCommand(t *testing.T, path string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, path, args...)
}

func TestPortableFixturePreservesArgvAndStdin(t *testing.T) {
	args := []string{"two words", "中文", `literal $() & | " text`}
	path := WriteCLI(t, "args", Spec{Mode: "echo-args"})
	raw, err := fixtureCommand(t, path, args...).Output()
	if err != nil || string(raw) != strings.Join(args, "\n")+"\n" {
		t.Fatalf("native argv fixture: %q %v", raw, err)
	}
	path = WriteCLI(t, "stdin", Spec{Mode: "echo-stdin"})
	cmd := fixtureCommand(t, path, "stdin")
	cmd.Stdin = strings.NewReader("controlled 中文\n")
	raw, err = cmd.Output()
	if err != nil || string(raw) != "mode:stdin\ncontrolled 中文\n" {
		t.Fatalf("native stdin fixture: %q %v", raw, err)
	}
}

func TestPortableFixtureProbeActuallyRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	path := WriteCLI(t, "probe", Spec{Mode: "probe", Sentinel: marker, Stdout: "fixture-version\n"})
	raw, err := fixtureCommand(t, path, "--version").Output()
	if err != nil || string(raw) != "fixture-version\n" {
		t.Fatalf("native probe: %q %v", raw, err)
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "called\n" {
		t.Fatalf("native probe marker: %q %v", raw, err)
	}
	if err := fixtureCommand(t, path, "unexpected").Run(); err == nil {
		t.Fatal("probe accepted unexpected argv")
	}
}

func TestPortableFixtureCounterActuallyStartsEachAttempt(t *testing.T) {
	state := filepath.Join(t.TempDir(), "attempts")
	path := WriteCLI(t, "counter", Spec{Mode: "counter", StateFile: state, FirstError: "quota exceeded", Stdout: "ok\n"})
	raw, err := fixtureCommand(t, path).CombinedOutput()
	if err == nil || string(raw) != "quota exceeded\n" {
		t.Fatalf("first native attempt: %q %v", raw, err)
	}
	raw, err = fixtureCommand(t, path).CombinedOutput()
	if err != nil || string(raw) != "ok\n" {
		t.Fatalf("second native attempt: %q %v", raw, err)
	}
	if raw, err := os.ReadFile(state); err != nil || string(raw) != "2\n" {
		t.Fatalf("attempt count: %q %v", raw, err)
	}
}

func TestPortableFixtureMissingSidecarCannotRunTests(t *testing.T) {
	path := WriteCLI(t, "missing-evidence", Spec{Stdout: "unused\n"})
	if err := os.Remove(path + ".fixture.json"); err != nil {
		t.Fatal(err)
	}
	raw, err := fixtureCommand(t, path).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 90 || len(raw) != 0 {
		t.Fatalf("missing fixture sidecar must fail closed without running tests: exit=%v output=%q", err, raw)
	}
}
