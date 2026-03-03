package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapPreviewProjectCommand_RequiresLinuxSandbox(t *testing.T) {
	oldGOOS := previewGOOS
	oldUnsafe := previewUnsafe
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
	})

	previewGOOS = "darwin"
	previewUnsafe = func() bool { return false }

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
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
	})

	previewGOOS = "linux"
	previewUnsafe = func() bool { return false }
	previewLookPath = func(string) (string, error) { return "", errors.New("missing") }

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
	t.Cleanup(func() {
		previewUnsafe = oldUnsafe
	})
	previewUnsafe = func() bool { return true }

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
	t.Cleanup(func() {
		previewGOOS = oldGOOS
		previewUnsafe = oldUnsafe
		previewLookPath = oldLookPath
	})

	previewGOOS = "linux"
	previewUnsafe = func() bool { return false }
	previewLookPath = func(string) (string, error) { return "/usr/bin/bwrap", nil }

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
}
