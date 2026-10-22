//go:build linux || openbsd || dragonfly || solaris

package router

import (
	"os"
	"syscall"
	"time"
)

// quotaIdentityFileVersion returns the device, inode and status change time
// (ctime) of the file info describes; ok is false when it carries none.
func quotaIdentityFileVersion(info os.FileInfo) (dev, ino uint64, changed time.Time, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, time.Time{}, false
	}
	return uint64(st.Dev), uint64(st.Ino), time.Unix(st.Ctim.Unix()), true
}
