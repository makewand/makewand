//go:build linux

package engine

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func tryReflink(in, out *os.File) error { return unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) }

func reflinkUnsupported(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EXDEV)
}

func workspaceCopyDevice(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Dev), true
}

func workspaceReflinkFilesystem(sourceRoot, targetRoot string) bool {
	var source, target unix.Statfs_t
	if unix.Statfs(sourceRoot, &source) != nil || unix.Statfs(targetRoot, &target) != nil || source.Fsid != target.Fsid || source.Type != target.Type {
		return false
	}
	// XFS also requires its reflink feature to be enabled; the first failed
	// ioctl still falls back completely and caches an unsupported device pair.
	return target.Type == unix.BTRFS_SUPER_MAGIC || target.Type == unix.XFS_SUPER_MAGIC
}
