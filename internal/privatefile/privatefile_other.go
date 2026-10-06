//go:build !unix && !windows

package privatefile

import (
	"fmt"
	"os"
)

// Tighten sets the platform's private permission bits on the fixed object.
func Tighten(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("private file is not regular")
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return Validate(file)
}

// Validate checks privacy without changing an existing object.
func Validate(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe private file permissions")
	}
	return nil
}
