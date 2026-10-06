//go:build !windows

package remotesession

import "os"

func readSessionFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
