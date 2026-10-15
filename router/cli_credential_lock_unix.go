//go:build !windows

package router

import (
	"os"
	"syscall"
)

// openCredentialLock opens, creating it, the sidecar lock file at path
// without following a symlink there (O_NOFOLLOW), as the Python engine's
// _credential_lock opens it: a symlink planted at the lock path makes the
// open fail instead of creating or locking the file it points to.
func openCredentialLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: sidecar of a provider credential path chosen by makewand
}
