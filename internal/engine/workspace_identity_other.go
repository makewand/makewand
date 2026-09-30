//go:build !unix && !windows

package engine

import (
	"fmt"
	"os"
)

func checkpointIdentity(string, os.FileInfo) (checkpointFileIdentity, error) {
	return checkpointFileIdentity{}, fmt.Errorf("regular file identity is unavailable on this platform")
}
