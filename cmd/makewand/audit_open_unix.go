//go:build unix

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

// Open the actual audit leaf without following a substituted symbolic link.
func openUnsafeHostExecAudit(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
}
