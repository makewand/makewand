//go:build unix

package backup

import (
	"os"

	"golang.org/x/sys/unix"
)

func openRestoreJournal(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open restore journal", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
