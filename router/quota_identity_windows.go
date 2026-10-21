//go:build windows

package router

import "os"

// quotaIdentityFileID returns no file index on Windows: the file info of an
// inspection carries none, and the stamp's size and modification time (100 ns
// resolution) identify the version.
func quotaIdentityFileID(os.FileInfo) (dev, ino uint64) {
	return 0, 0
}
