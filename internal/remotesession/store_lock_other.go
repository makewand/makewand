//go:build !unix && !windows

package remotesession

import (
	"fmt"
	"os"
)

func openStoreLock(string) (*os.File, error) {
	return nil, fmt.Errorf("session store locking is unavailable on this platform")
}
func lockStoreHandle(*os.File, bool) error {
	return fmt.Errorf("session store locking is unavailable on this platform")
}
func tryExclusiveStoreLock(*os.File) (bool, error) {
	return false, fmt.Errorf("session store locking is unavailable on this platform")
}
func syncStoreDirectory(string) error {
	return fmt.Errorf("session store directory durability is unavailable on this platform")
}
