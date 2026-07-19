package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// swapVerifyIsolationVars installs fake isolation probes and restores the
// originals on cleanup so unit tests never depend on bubblewrap being
// installed on the host.
func swapVerifyIsolationVars(t *testing.T) {
	t.Helper()
	oldGOOS := verifyGOOS
	oldUnsafe := verifyUnsafe
	oldLookPath := verifyLookPath
	oldUserHome := verifyUserHome
	oldGetenv := verifyGetenv
	oldSelfTest := verifyBwrapSelfTest
	t.Cleanup(func() {
		verifyGOOS = oldGOOS
		verifyUnsafe = oldUnsafe
		verifyLookPath = oldLookPath
		verifyUserHome = oldUserHome
		verifyGetenv = oldGetenv
		verifyBwrapSelfTest = oldSelfTest
	})
}

// fakeWorkingBwrap simulates a Linux host with functional bubblewrap.
func fakeWorkingBwrap(t *testing.T) {
	t.Helper()
	swapVerifyIsolationVars(t)
	verifyGOOS = "linux"
	verifyUnsafe = func() bool { return false }
	verifyLookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	verifyUserHome = func() (string, error) { return "/home/alice", nil }
	verifyBwrapSelfTest = func(string) error { return nil }
	verifyGetenv = func(key string) string {
		if key == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}
}

// fakeMissingBwrap simulates a Linux host without bubblewrap and without the
// unsafe host-exec opt-in: verification must fail closed.
func fakeMissingBwrap(t *testing.T) {
	t.Helper()
	swapVerifyIsolationVars(t)
	verifyGOOS = "linux"
	verifyUnsafe = func() bool { return false }
	verifyLookPath = func(string) (string, error) { return "", errors.New("missing") }
	verifyBwrapSelfTest = func(string) error { return nil }
}

func TestResolveVerifyExecEnvironment_RequiresLinux(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGOOS = "darwin"
	verifyUnsafe = func() bool { return false }

	_, err := resolveVerifyExecEnvironment()
	if err == nil {
		t.Fatal("expected error on non-linux without unsafe bypass")
	}
	if !strings.Contains(err.Error(), "MAKEWAND_UNSAFE_HOST_EXEC") {
		t.Fatalf("error=%q, want unsafe bypass hint", err.Error())
	}
}

func TestResolveVerifyExecEnvironment_RequiresBwrap(t *testing.T) {
	fakeMissingBwrap(t)

	_, err := resolveVerifyExecEnvironment()
	if err == nil {
		t.Fatal("expected error when bwrap is unavailable")
	}
	if !strings.Contains(err.Error(), "bubblewrap") {
		t.Fatalf("error=%q, want bubblewrap hint", err.Error())
	}
	if RestrictedExecAutoApprovable() {
		t.Fatal("RestrictedExecAutoApprovable() = true, want false without isolation")
	}
	if RestrictedExecIsolationError() == nil {
		t.Fatal("RestrictedExecIsolationError() = nil, want reason")
	}
}

func TestResolveVerifyExecEnvironment_SelfTestFailure(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGOOS = "linux"
	verifyUnsafe = func() bool { return false }
	verifyLookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	verifyBwrapSelfTest = func(string) error { return errors.New("setting up uid map: Permission denied") }

	_, err := resolveVerifyExecEnvironment()
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

func TestResolveVerifyExecEnvironment_UnsafeBypass(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGOOS = "darwin" // even without a sandbox-capable OS
	verifyUnsafe = func() bool { return true }

	env, err := resolveVerifyExecEnvironment()
	if err != nil {
		t.Fatalf("resolveVerifyExecEnvironment: %v", err)
	}
	if env.mode != verifyExecUnsafeHost {
		t.Fatalf("mode = %v, want verifyExecUnsafeHost", env.mode)
	}
	if VerificationIsolationActive() {
		t.Fatal("VerificationIsolationActive() = true, want false in unsafe host mode")
	}
	if !RestrictedExecAutoApprovable() {
		t.Fatal("RestrictedExecAutoApprovable() = false, want true with explicit opt-in")
	}
}

func TestResolveVerifyExecEnvironment_IsolatedMode(t *testing.T) {
	fakeWorkingBwrap(t)

	env, err := resolveVerifyExecEnvironment()
	if err != nil {
		t.Fatalf("resolveVerifyExecEnvironment: %v", err)
	}
	if env.mode != verifyExecIsolated || env.bwrapPath != "/usr/bin/bwrap" {
		t.Fatalf("env = %+v, want isolated bwrap environment", env)
	}
	if !VerificationIsolationActive() {
		t.Fatal("VerificationIsolationActive() = false, want true")
	}
}

func TestWrapVerificationCommand_NetworkIsolationByStep(t *testing.T) {
	fakeWorkingBwrap(t)

	tests := []struct {
		name         string
		allowNetwork bool
		wantUnshare  bool
	}{
		{name: "test step is network isolated", allowNetwork: false, wantUnshare: true},
		{name: "dependency install keeps network", allowNetwork: true, wantUnshare: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, args := wrapVerificationCommand("/usr/bin/bwrap", "/tmp/demo", "go", []string{"test", "./..."}, tt.allowNetwork)
			if cmd != "/usr/bin/bwrap" {
				t.Fatalf("cmd = %q, want /usr/bin/bwrap", cmd)
			}
			if got := containsArg(args, "--unshare-net"); got != tt.wantUnshare {
				t.Fatalf("--unshare-net present = %v, want %v (args=%v)", got, tt.wantUnshare, args)
			}
		})
	}
}

func TestWrapVerificationCommand_SandboxLayout(t *testing.T) {
	fakeWorkingBwrap(t)

	workspace := "/tmp/makewand-candidate-123"
	_, args := wrapVerificationCommand("/usr/bin/bwrap", workspace, "go", []string{"test", "./..."}, false)

	if !containsArg(args, "--clearenv") {
		t.Fatalf("args should clear inherited environment; got %v", args)
	}
	if !containsArg(args, "--die-with-parent") {
		t.Fatalf("args should include --die-with-parent; got %v", args)
	}
	if !containsArgPair(args, "--ro-bind", "/") {
		t.Fatalf("args should read-only bind root; got %v", args)
	}
	if !containsArgPair(args, "PATH", "/usr/bin:/bin") {
		t.Fatalf("args should set PATH; got %v", args)
	}
	wantHome := filepath.Join(workspace, ".makewand", "sandbox-home")
	if !containsArgPair(args, "HOME", wantHome) {
		t.Fatalf("args should set HOME under the workspace (%s); got %v", wantHome, args)
	}
	if !containsArgPair(args, "--tmpfs", "/home/alice") {
		t.Fatalf("args should mask host HOME; got %v", args)
	}
	if len(args) < 3 || args[len(args)-3] != "go" {
		t.Fatalf("wrapped args should end with the original command; got %v", args)
	}

	// The workspace bind must come after tmpfs /tmp so temp-dir clones stay
	// writable inside the sandbox.
	tmpfsIdx, bindIdx := -1, -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--tmpfs" && args[i+1] == "/tmp" {
			tmpfsIdx = i
		}
		if args[i] == "--bind" && args[i+1] == workspace {
			bindIdx = i
		}
	}
	if tmpfsIdx == -1 || bindIdx == -1 || bindIdx < tmpfsIdx {
		t.Fatalf("workspace bind (idx %d) must follow tmpfs /tmp (idx %d); args=%v", bindIdx, tmpfsIdx, args)
	}
}

func TestVerificationPlanAllowsNetwork(t *testing.T) {
	tests := []struct {
		name string
		plan ExecPlan
		want bool
	}{
		{name: "deps install", plan: ExecPlan{Kind: "deps", Command: "npm", Args: []string{"install", "--ignore-scripts"}}, want: true},
		{name: "tests", plan: ExecPlan{Kind: "tests", Command: "go", Args: []string{"test", "./..."}}, want: false},
		{name: "quick check", plan: ExecPlan{Kind: "quickcheck", Command: "python3", Args: []string{"-m", "py_compile"}}, want: false},
	}

	for _, tt := range tests {
		if got := verificationPlanAllowsNetwork(tt.plan); got != tt.want {
			t.Fatalf("%s: verificationPlanAllowsNetwork() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
