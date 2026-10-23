//go:build !linux && !openbsd && !dragonfly && !solaris && !darwin && !freebsd && !netbsd

package router

import (
	"os"
	"time"
)

// quotaIdentityFileVersion reports no version: the file info here (Windows)
// carries no change time, and a rewrite can keep the size and modification
// time, so every identity request reads the file.
func quotaIdentityFileVersion(os.FileInfo) (dev, ino uint64, changed time.Time, ok bool) {
	return 0, 0, time.Time{}, false
}
