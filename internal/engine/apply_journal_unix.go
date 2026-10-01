//go:build !windows

package engine

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func privateApplyDirectory(path string) error {
	//nolint:gosec // G703: callers pass canonical, external private state directories, never generated file paths.
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("apply state must be a private directory")
	}
	//nolint:gosec // G703: validated external private state directory; permissions are tightened, not inherited from a candidate.
	return os.Chmod(path, 0o700)
}

func openApplyLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("apply lock must be a regular file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || int64(stat.Uid) != int64(os.Geteuid()) {
		file.Close()
		return nil, fmt.Errorf("apply lock must be owned by this user and have one link")
	}
	return file, nil
}

func supportedApplyMode(mode os.FileMode) os.FileMode { return mode.Perm() }

func replaceApplyFile(root *os.Root, source, destination string) error {
	return root.Rename(source, destination)
}

func removeApplyFile(root *os.Root, path string) error { return root.Remove(path) }

func applyWorkspaceIdentity(path string) (string, error) {
	//nolint:gosec // G703: the host selects this canonical workspace; reading its directory identity authorizes no file writes.
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("apply workspace must be a real directory")
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}

func applyOpenRootIdentity(root *os.Root) (string, error) {
	info, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return "", fmt.Errorf("apply root must hold a directory")
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}

func tryApplyLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockApply(file *os.File) error { return syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }

func syncApplyDirectory(path string) error {
	//nolint:gosec // G703: callers supply their validated workspace/state directory for a read-only durability flush.
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func applyOpenFileSecurity(*os.File) (string, error)  { return "", nil }
func validateApplySecurity(string) error              { return nil }
func applySetOpenFileSecurity(*os.File, string) error { return nil }
