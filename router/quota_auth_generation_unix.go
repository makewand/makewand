//go:build !windows

package router

import "os"

func quotaAuthGeneration(_ string, info os.FileInfo) ([4]uint64, bool) {
	dev, ino, changed, known := quotaIdentityFileVersion(info)
	seconds := changed.Unix()
	nanos := changed.Nanosecond()
	if !known || seconds < 0 || nanos < 0 {
		return [4]uint64{}, false
	}
	return [4]uint64{dev, ino, uint64(seconds), uint64(nanos)}, true
}
