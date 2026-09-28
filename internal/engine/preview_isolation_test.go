package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrapPreviewProjectCommand_RequiresLinuxSandbox(t *testing.T) {
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	oldSelfTest := previewBwrapSelfTest
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewBwrapSelfTest = oldSelfTest
	})

	previewGOOS = "darwin"
	previewUnsafe = func() bool { return false }
	previewBwrapSelfTest = func(string) error { return nil }

	_, _, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{})
	if err == nil {
		t.Fatal("expected error on non-linux without unsafe bypass")
	}
	if !strings.Contains(err.Error(), "MAKEWAND_UNSAFE_HOST_EXEC") {
		t.Fatalf("error=%q, want unsafe bypass hint", err.Error())
	}
}

func TestWrapPreviewProjectCommand_RequiresBwrapOnLinux(t *testing.T) {
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	oldLookPath := previewLookPath
	oldSelfTest := previewBwrapSelfTest
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
		previewBwrapSelfTest = oldSelfTest
	})

	previewGOOS = "linux"
	previewUnsafe = func() bool { return false }
	previewLookPath = func(string) (string, error) { return "", errors.New("missing") }
	previewBwrapSelfTest = func(string) error { return nil }

	_, _, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{})
	if err == nil {
		t.Fatal("expected error when bwrap is unavailable")
	}
	if !strings.Contains(err.Error(), "bubblewrap") {
		t.Fatalf("error=%q, want bubblewrap hint", err.Error())
	}
}

func TestWrapPreviewProjectCommand_UnsafeBypass(t *testing.T) {
	oldUnsafe := previewUnsafe
	oldSelfTest := previewBwrapSelfTest
	t.Cleanup(func() {
		previewUnsafe = oldUnsafe
		previewBwrapSelfTest = oldSelfTest
	})
	previewUnsafe = func() bool { return true }
	previewBwrapSelfTest = func(string) error { return nil }

	var audited []UnsafeHostExecEvent
	auth := UnsafeHostExecAuthorization{
		Acknowledged: true,
		Source:       "test",
		Audit:        func(ev UnsafeHostExecEvent) { audited = append(audited, ev) },
	}
	cmd, args, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"}, auth)
	if err != nil {
		t.Fatalf("wrapPreviewProjectCommand: %v", err)
	}
	if cmd != "npm" {
		t.Fatalf("cmd=%q, want npm", cmd)
	}
	if len(args) != 2 || args[0] != "run" || args[1] != "dev" {
		t.Fatalf("args=%v, want [run dev]", args)
	}
	if len(audited) != 1 || audited[0].Context != "preview" || audited[0].Command != "npm" || audited[0].Source != "test" {
		t.Fatalf("audited=%+v, want one preview event for npm with source test", audited)
	}
}

// TestWrapPreviewProjectCommand_UnsafeRequestedWithoutAck confirms the env
// opt-in alone does not bypass preview isolation: without the acknowledged
// authorization the wrapper behaves as if the variable were unset (isolation
// on capable hosts, fail-closed with acknowledgment guidance otherwise).
func TestWrapPreviewProjectCommand_UnsafeRequestedWithoutAck(t *testing.T) {
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	oldLookPath := previewLookPath
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
	})
	previewGOOS = "darwin"
	previewUnsafe = func() bool { return true }

	_, _, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{})
	if err == nil {
		t.Fatal("expected fail-closed error when opt-in is requested but unacknowledged")
	}
	if !strings.Contains(err.Error(), "acknowledg") {
		t.Fatalf("error=%q, want acknowledgment guidance", err.Error())
	}
}

// fakePreviewSandbox installs a Linux host with working bubblewrap and a real
// (temporary) HOME for the preview wrapper, restoring everything afterwards.
func fakePreviewSandbox(t *testing.T) string {
	t.Helper()
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	oldLookPath := previewLookPath
	oldUserHome := previewUserHome
	oldGetenv := previewGetenv
	oldSelfTest := previewBwrapSelfTest
	oldGoEnvProbe := sandboxGoEnvProbe
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
		previewUserHome = oldUserHome
		previewGetenv = oldGetenv
		previewBwrapSelfTest = oldSelfTest
		sandboxGoEnvProbe = oldGoEnvProbe
	})

	home := t.TempDir()
	previewGOOS = "linux"
	previewUnsafe = func() bool { return false }
	previewLookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	previewUserHome = func() (string, error) { return home, nil }
	previewBwrapSelfTest = func(string) error { return nil }
	previewGetenv = func(key string) string {
		if key == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}
	sandboxGoEnvProbe = func(string) (hostGoEnv, error) { return hostGoEnv{}, errors.New("go env disabled in unit tests") }
	return home
}

func TestWrapPreviewProjectCommand_BwrapWrapsCommand(t *testing.T) {
	home := fakePreviewSandbox(t)

	cmd, args, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{})
	if err != nil {
		t.Fatalf("wrapPreviewProjectCommand: %v", err)
	}
	if cmd != "/usr/bin/bwrap" {
		t.Fatalf("cmd=%q, want /usr/bin/bwrap", cmd)
	}
	if len(args) == 0 || args[len(args)-3] != "npm" {
		t.Fatalf("wrapped args should contain original command; got %v", args)
	}
	if !containsArg(args, "--clearenv") {
		t.Fatalf("wrapped args should clear inherited environment; got %v", args)
	}
	if !containsArgPair(args, "PATH", "/usr/bin:/bin") {
		t.Fatalf("wrapped args should set PATH; got %v", args)
	}
	if !containsArgPair(args, "HOME", "/tmp") {
		t.Fatalf("wrapped args should set HOME=/tmp; got %v", args)
	}
	if !containsArgPair(args, "--tmpfs", home) {
		t.Fatalf("wrapped args should mask host HOME; got %v", args)
	}
}

// A project under HOME used to get per-entry masks (--tmpfs HOME/.ssh, ...):
// missing or file entries made bwrap fail with "Can't mkdir", and everything
// not on the list (AI CLI credentials, .git-credentials, ...) stayed readable.
// HOME is now hidden wholesale BEFORE the project bind re-exposes the project.
func TestWrapPreviewProjectCommand_HidesWholeHomeWhenProjectInsideHome(t *testing.T) {
	home := fakePreviewSandbox(t)
	projectPath := filepath.Join(home, "work", "demo")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine x password y\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, args, err := wrapPreviewProjectCommand(projectPath, "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{})
	if err != nil {
		t.Fatalf("wrapPreviewProjectCommand: %v", err)
	}
	tmpfsIdx := argPairIndex(args, "--tmpfs", home)
	bindIdx := argPairIndex(args, "--bind", projectPath)
	if tmpfsIdx < 0 || bindIdx < 0 || tmpfsIdx > bindIdx {
		t.Fatalf("HOME tmpfs (idx %d) must precede the project bind (idx %d): %v", tmpfsIdx, bindIdx, args)
	}
	for _, entry := range []string{".ssh", ".aws", ".netrc", ".npmrc"} {
		target := filepath.Join(home, entry)
		if containsArgPair(args, "--tmpfs", target) || containsArgPair(args, os.DevNull, target) {
			t.Fatalf("per-entry mask for %s is unnecessary (and breaks on missing/file entries): %v", entry, args)
		}
	}
}

func TestPathWithin(t *testing.T) {
	base := t.TempDir()
	child := base + string(os.PathSeparator) + "child"
	if !pathWithin(base, base) {
		t.Fatalf("pathWithin(%q, %q)=false, want true", base, base)
	}
	if !pathWithin(base, child) {
		t.Fatalf("pathWithin(%q, %q)=false, want true", base, child)
	}
	if pathWithin(base, "/tmp") {
		t.Fatalf("pathWithin(%q, %q)=true, want false", base, "/tmp")
	}
}

func TestWrapPreviewProjectCommand_BwrapSelfTestFailure(t *testing.T) {
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	oldLookPath := previewLookPath
	oldSelfTest := previewBwrapSelfTest
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
		previewBwrapSelfTest = oldSelfTest
	})

	previewGOOS = "linux"
	previewUnsafe = func() bool { return false }
	previewLookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	previewBwrapSelfTest = func(string) error { return errors.New("setting up uid map: Permission denied") }

	_, _, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{})
	if err == nil {
		t.Fatal("expected bwrap self-test error")
	}
	if !strings.Contains(err.Error(), "uid map") {
		t.Fatalf("error=%q, want uid map detail", err.Error())
	}
	if !strings.Contains(err.Error(), "MAKEWAND_UNSAFE_HOST_EXEC=1") {
		t.Fatalf("error=%q, want unsafe bypass hint", err.Error())
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// argPairIndex returns the index of the first "left right" pair, or -1.
func argPairIndex(args []string, left, right string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == left && args[i+1] == right {
			return i
		}
	}
	return -1
}

func containsArgPair(args []string, left, right string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == left && args[i+1] == right {
			return true
		}
	}
	return false
}
