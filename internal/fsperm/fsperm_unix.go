//go:build !windows

package fsperm

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// UntraversableForeignDir reports whether path is a directory owned by another
// user that the current user cannot enter. A sandbox running as the same uid
// can neither enter nor chmod such a directory, so nothing below it (sockets,
// .git) is reachable from inside and a scanner may skip it. Directories the
// current user owns are never reported: a sandboxed process could chmod them
// back.
func UntraversableForeignDir(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == os.Geteuid() {
		return false
	}
	return unix.Access(path, unix.X_OK) != nil
}
