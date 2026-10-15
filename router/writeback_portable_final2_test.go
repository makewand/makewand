package router

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writebackFixHostPath is the path a write-back warning names for the host
// credential file host: copyIn resolves the symbolic links in it, or in its
// directory when the file does not exist. Temporary directories differ from
// their resolved form on macOS (/var is /private/var) and on Windows (8.3
// short names such as RUNNER~1 in TEMP).
func writebackFixHostPath(host string) string {
	if real, err := filepath.EvalSymlinks(host); err == nil {
		return real
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(host)); err == nil {
		return filepath.Join(dir, filepath.Base(host))
	}
	return host
}

// writebackFixNamesHost reports whether warning names the host credential
// file host, as given or resolved.
func writebackFixNamesHost(warning, host string) bool {
	return strings.Contains(warning, host) || strings.Contains(warning, writebackFixHostPath(host))
}

// writebackFixOwnerOnly reports whether a written host credential has the
// mode a write-back gives it: 0600 where permission bits control access. On
// Windows ACLs do, and Go reports 0666 for every file that is not read-only,
// so only that the file stayed writable is checked there.
func writebackFixOwnerOnly(fi os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return fi.Mode().Perm()&0o200 != 0
	}
	return fi.Mode().Perm() == 0o600
}

// canonicalTempDir is t.TempDir with its symbolic links resolved. Sandbox
// mount plans name the canonical paths of what they bind (a selected
// CODEX_HOME, the private copies of credential directories), so tests that
// look for them start from canonical paths too: TMPDIR itself may be a
// symbolic link (macOS's /var/folders is /private/var/folders).
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
