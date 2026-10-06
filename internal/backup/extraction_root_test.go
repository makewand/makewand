package backup

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func extractionFixture(t *testing.T, name, content string) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "fixture.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	return archive
}

func TestExtractionRootPreservesNestedSessionNames(t *testing.T) {
	dest := t.TempDir()
	name := "sessions/tenant/profile..json"
	if err := extractTarGz(extractionFixture(t, name, "session"), dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name))); err != nil || string(got) != "session" {
		t.Fatalf("nested extraction = %q, %v", got, err)
	}
}

func TestExtractionRootRejectsExistingFileWithoutTruncating(t *testing.T) {
	dest := t.TempDir()
	path := filepath.Join(dest, "state.db")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractTarGz(extractionFixture(t, "state.db", "replacement"), dest); err == nil {
		t.Fatal("extraction overwrote an existing file")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "existing" {
		t.Fatalf("existing file changed = %q, %v", got, err)
	}
}

func TestExtractionRootRejectsEscapingSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs Windows privileges")
	}
	for _, parentLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "leaf", true: "parent"}[parentLink], func(t *testing.T) {
			dest, outside := t.TempDir(), t.TempDir()
			victim := filepath.Join(outside, "state.db")
			if err := os.WriteFile(victim, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			name := "state.db"
			if parentLink {
				name = "sessions/state.db"
				if err := os.Symlink(outside, filepath.Join(dest, "sessions")); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(victim, filepath.Join(dest, name)); err != nil {
				t.Fatal(err)
			}
			if err := extractTarGz(extractionFixture(t, name, "replacement"), dest); err == nil {
				t.Fatal("extraction followed a link outside its root")
			}
			if got, err := os.ReadFile(victim); err != nil || string(got) != "outside" {
				t.Fatalf("outside file changed = %q, %v", got, err)
			}
		})
	}
}

func TestExtractionRootRemovesOnlyItsOversizedEntry(t *testing.T) {
	old := maxArchiveEntrySize
	maxArchiveEntrySize = 3
	t.Cleanup(func() { maxArchiveEntrySize = old })
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractTarGz(extractionFixture(t, "sessions/oversized", "oversized"), dest); err == nil {
		t.Fatal("oversized entry accepted")
	}
	if _, err := os.Stat(filepath.Join(dest, "sessions", "oversized")); !os.IsNotExist(err) {
		t.Fatalf("oversized file remains: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "keep")); err != nil || string(got) != "keep" {
		t.Fatalf("unrelated file changed = %q, %v", got, err)
	}
}
