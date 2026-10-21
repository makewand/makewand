//go:build freebsd

package remotesession

import "syscall"

// diskFreeBytes returns the bytes an unprivileged writer can still use on
// dir's filesystem.
func diskFreeBytes(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	// Bavail is signed here: it goes negative once usage passes the UFS
	// reserve, which root can still write into, and nothing is then free.
	if stat.Bavail <= 0 {
		return 0, nil
	}
	return uint64(stat.Bavail) * stat.Bsize, nil //nolint:gosec // G115: Bavail is positive here.
}
