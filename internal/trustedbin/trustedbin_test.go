//go:build !windows

package trustedbin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeExec(t *testing.T, dir, name string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // G306: fixture must be executable
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

func withSystemDirs(t *testing.T, dirs ...string) {
	t.Helper()
	old := SystemDirs
	SystemDirs = dirs
	t.Cleanup(func() { SystemDirs = old })
}

func TestLookPathIgnoresPATH(t *testing.T) {
	planted := t.TempDir()
	writeExec(t, planted, "bwrap", 0o755)
	t.Setenv("PATH", planted)
	t.Setenv(OverrideEnv("bwrap"), "")
	withSystemDirs(t, t.TempDir()) // empty trusted dir

	if got, err := LookPath("bwrap"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookPath = %q, %v; want ErrNotFound (PATH must not be consulted)", got, err)
	}
}

func TestLookPathRejectsNonRootSystemDirEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: temp files are root-owned")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, dir, "bwrap", 0o755)
	t.Setenv(OverrideEnv("bwrap"), "")
	withSystemDirs(t, dir)

	if _, err := LookPath("bwrap"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user-owned binary in search dir accepted: %v", err)
	}
}

func TestLookPathOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := writeExec(t, dir, "bwrap", 0o755)
	withSystemDirs(t)

	t.Setenv(OverrideEnv("bwrap"), good)
	got, err := LookPath("bwrap")
	if err != nil {
		t.Fatalf("owner-controlled override rejected: %v", err)
	}
	want, _ := filepath.EvalSymlinks(good)
	if got != want {
		t.Fatalf("LookPath = %q, want resolved %q", got, want)
	}

	t.Setenv(OverrideEnv("bwrap"), "bwrap")
	if _, err := LookPath("bwrap"); err == nil {
		t.Fatal("relative override accepted")
	}

	writable := writeExec(t, dir, "bwrap-ww", 0o777)
	t.Setenv(OverrideEnv("bwrap"), writable)
	if _, err := LookPath("bwrap"); err == nil {
		t.Fatal("world-writable override accepted")
	}

	wwDir := t.TempDir()
	if err := os.Chmod(wwDir, 0o777); err != nil {
		t.Fatal(err)
	}
	inWW := writeExec(t, wwDir, "bwrap", 0o755)
	t.Setenv(OverrideEnv("bwrap"), inWW)
	if _, err := LookPath("bwrap"); err == nil {
		t.Fatal("override inside world-writable directory accepted")
	}
}

func TestLookPathFindsSystemBwrap(t *testing.T) {
	t.Setenv(OverrideEnv("bwrap"), "")
	for _, dir := range SystemDirs {
		if _, err := os.Stat(filepath.Join(dir, "bwrap")); err == nil {
			got, err := LookPath("bwrap")
			if err != nil {
				t.Fatalf("system bwrap in %s rejected: %v", dir, err)
			}
			if !filepath.IsAbs(got) {
				t.Fatalf("non-absolute result %q", got)
			}
			return
		}
	}
	t.Skip("no system bwrap installed")
}

func TestLookPathRejectsPathSeparators(t *testing.T) {
	if _, err := LookPath("../bwrap"); err == nil {
		t.Fatal("name with separator accepted")
	}
}
