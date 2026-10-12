//go:build !windows

package fsperm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUntraversableForeignDir(t *testing.T) {
	own := filepath.Join(t.TempDir(), "own")
	if err := os.Mkdir(own, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(own, 0o700) })
	if UntraversableForeignDir(own) {
		t.Fatal("own unreadable directory reported as foreign; a sandbox could chmod it back")
	}
	if UntraversableForeignDir(t.TempDir()) {
		t.Fatal("traversable directory reported")
	}
	if os.Geteuid() == 0 {
		t.Skip("root traverses every directory")
	}
	// /root is root-owned and closed to other users on standard systems.
	if fi, err := os.Stat("/root"); err == nil && fi.Mode().Perm()&0o001 == 0 {
		if !UntraversableForeignDir("/root") {
			t.Fatal("/root not reported as an untraversable foreign directory")
		}
	}
}
