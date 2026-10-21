//go:build !windows

package router

import (
	"os"
	"syscall"
)

// quotaIdentityFileID returns the device and inode of the file info describes.
func quotaIdentityFileID(info os.FileInfo) (dev, ino uint64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}
