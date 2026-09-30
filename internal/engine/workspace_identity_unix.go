//go:build unix

package engine

import (
	"fmt"
	"os"
	"syscall"
)

func checkpointIdentity(_ string, info os.FileInfo) (checkpointFileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Ino == 0 || stat.Nlink == 0 {
		return checkpointFileIdentity{}, fmt.Errorf("regular file identity is unavailable")
	}
	return checkpointFileIdentity{Nlink: uint64(stat.Nlink), Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, nil
}
