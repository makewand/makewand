//go:build linux || darwin || freebsd

package remotesession

import "syscall"

// diskFreeBytes returns the bytes an unprivileged writer can still use on
// dir's filesystem.
func diskFreeBytes(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil //nolint:gosec // G115: block counts and sizes are non-negative.
}
