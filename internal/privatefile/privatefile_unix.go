//go:build unix

package privatefile

import (
	"fmt"
	"os"
	"syscall"
)

func validateObject(file *os.File) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || int64(stat.Uid) != int64(os.Geteuid()) {
		return nil, fmt.Errorf("private file must be a regular single-link file owned by the current user")
	}
	return info, nil
}

// Tighten sets privacy on the same opened object before callers write secrets.
func Tighten(file *os.File) error {
	if _, err := validateObject(file); err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return Validate(file)
}

// Validate checks privacy without repairing an untrusted existing file.
func Validate(file *os.File) error {
	info, err := validateObject(file)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("private file permissions allow another account")
	}
	return nil
}
