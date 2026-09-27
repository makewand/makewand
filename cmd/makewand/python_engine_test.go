package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writePythonLauncher(t *testing.T, root string) string {
	t.Helper()
	for _, path := range []string{"bin/makewand", "makewand/__init__.py"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# installed engine\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "bin", "makewand")
}

func TestPythonLauncherPrefersBundledEngine(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "release", "makewand")
	want := writePythonLauncher(t, filepath.Join(filepath.Dir(exe), "lib", "makewand", "python"))
	home := filepath.Join(root, "home")
	writePythonLauncher(t, filepath.Join(home, ".local", "share", "makewand"))
	if got := findPythonLauncher(exe, "", home); got != want {
		t.Fatalf("launcher = %q; want bundled %q", got, want)
	}
}

func TestPythonLauncherRequiresCompleteEngine(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: executable fixture proves an unrelated wrapper cannot qualify as the installed Python engine.
	if err := os.WriteFile(filepath.Join(root, "bin", "makewand"), []byte("unrelated command"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := findPythonLauncher(filepath.Join(root, "bin", "makewand-server"), root, ""); got != "" {
		t.Fatalf("selected launcher without engine: %q", got)
	}
	want := writePythonLauncher(t, root)
	if got := findPythonLauncher(filepath.Join(root, "bin", "makewand-server"), root, ""); got != want {
		t.Fatalf("launcher = %q; want %q", got, want)
	}
}
