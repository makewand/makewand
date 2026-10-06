//go:build !windows

package remotesession

import "os"

func replaceSessionFile(_ string, source, destination string) error {
	// #nosec G703 -- Save supplies a CreateTemp source and SHA-256 destination basename in the operator-configured storage root.
	return os.Rename(source, destination)
}

func removeSessionFile(_ string, path string) error {
	// #nosec G703 -- Store.Delete supplies pathFor's SHA-256 basename in the operator-configured storage root.
	return os.Remove(path)
}
