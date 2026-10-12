//go:build linux

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

func platformApplyRenameExchange(root *os.Root, source, destination string) error {
	return applyRenameat2(root, source, destination, unix.RENAME_EXCHANGE)
}

func platformApplyRenameNoReplace(root *os.Root, source, destination string) error {
	return applyRenameat2(root, source, destination, unix.RENAME_NOREPLACE)
}

// applyRenameat2 renames two siblings relative to their parent directory, which
// is opened through the workspace root so it cannot escape it.
func applyRenameat2(root *os.Root, source, destination string, flags uint) error {
	dir := filepath.Dir(source)
	if filepath.Dir(destination) != dir {
		return errApplyAtomicUnsupported
	}
	parent, err := root.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer parent.Close()
	fd := int(parent.Fd())
	err = unix.Renameat2(fd, filepath.Base(source), fd, filepath.Base(destination), flags)
	runtime.KeepAlive(parent)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		return errApplyAtomicUnsupported
	}
	if err != nil {
		return &os.LinkError{Op: "renameat2", Old: source, New: destination, Err: err}
	}
	return nil
}
