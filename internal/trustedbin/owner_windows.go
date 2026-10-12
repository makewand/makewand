//go:build windows

package trustedbin

import (
	"fmt"
	"os"
)

// bubblewrap does not exist on Windows; refuse rather than trust ACL-less checks.
func checkOwnership(path string, _ os.FileInfo, _ bool) error {
	return fmt.Errorf("%s: trusted launcher resolution is unsupported on Windows", path)
}
