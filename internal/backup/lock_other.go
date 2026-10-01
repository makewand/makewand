//go:build !unix && !windows

package backup

import (
	"fmt"
	"os"
)

func acquireRestoreLock(string) (*os.File, error) {
	return nil, fmt.Errorf("restore locking is unavailable on this platform")
}
func syncDirectory(string) error {
	return fmt.Errorf("restore directory durability is unavailable on this platform")
}
