package engine

import (
	"errors"
	"os"
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

	_, _, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"})
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

	_, _, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"})
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

	cmd, args, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"})
	if err != nil {
		t.Fatalf("wrapPreviewProjectCommand: %v", err)
	}
	if cmd != "npm" {
		t.Fatalf("cmd=%q, want npm", cmd)
	}
	if len(args) != 2 || args[0] != "run" || args[1] != "dev" {
		t.Fatalf("args=%v, want [run dev]", args)
	}
}

func TestWrapPreviewProjectCommand_BwrapWrapsCommand(t *testing.T) {
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	oldLookPath := previewLookPath
	oldUserHome := previewUserHome
	oldGetenv := previewGetenv
	oldSelfTest := previewBwrapSelfTest
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
		previewUserHome = oldUserHome
		previewGetenv = oldGetenv
		previewBwrapSelfTest = oldSelfTest
	})

	previewGOOS = "linux"
	previewUnsafe = func() bool { return false }
	previewLookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	previewUserHome = func() (string, error) { return "/home/alice", nil }
	previewBwrapSelfTest = func(string) error { return nil }
	previewGetenv = func(key string) string {
		if key == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}

	cmd, args, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"})
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
	if !containsArgPair(args, "--tmpfs", "/home/alice") {
		t.Fatalf("wrapped args should mask host HOME; got %v", args)
	}
}

func TestWrapPreviewProjectCommand_MasksSensitiveHomeSubpathsWhenProjectInsideHome(t *testing.T) {
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	oldLookPath := previewLookPath
	oldUserHome := previewUserHome
	oldGetenv := previewGetenv
	oldSelfTest := previewBwrapSelfTest
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
		previewUserHome = oldUserHome
		previewGetenv = oldGetenv
		previewBwrapSelfTest = oldSelfTest
	})

	previewGOOS = "linux"
	previewUnsafe = func() bool { return false }
	previewLookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	previewUserHome = func() (string, error) { return "/home/alice", nil }
	previewBwrapSelfTest = func(string) error { return nil }
	previewGetenv = func(key string) string {
		if key == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}

	projectPath := "/home/alice/work/demo"
	_, args, err := wrapPreviewProjectCommand(projectPath, "npm", []string{"run", "dev"})
	if err != nil {
		t.Fatalf("wrapPreviewProjectCommand: %v", err)
	}
	if containsArgPair(args, "--tmpfs", "/home/alice") {
		t.Fatalf("project under HOME should not mask entire HOME; got %v", args)
	}
	if !containsArgPair(args, "--tmpfs", "/home/alice/.ssh") {
		t.Fatalf("expected sensitive HOME path mask for .ssh; got %v", args)
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

	_, _, err := wrapPreviewProjectCommand("/tmp/demo", "npm", []string{"run", "dev"})
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

func containsArgPair(args []string, left, right string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == left && args[i+1] == right {
			return true
		}
	}
	return false
}
