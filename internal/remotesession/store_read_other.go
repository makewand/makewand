//go:build !windows

package remotesession

import "os"

func readSessionFile(path string) ([]byte, error) {
	// #nosec G703 -- callers pass sessionPath, a hashed basename in the operator-configured storage root, never a client-supplied path.
	return os.ReadFile(path)
}
