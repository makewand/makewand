//go:build windows

package remotesession

import (
	"os"
	"path/filepath"
)

func replaceSessionFile(directory, source, destination string) error {
	// #nosec G703 -- directory is the operator-configured session storage root, independent of client workspace IDs.
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	// Save creates its temporary source and hashed destination in this root.
	// Go's Windows Root.Rename requests POSIX replacement semantics, so an
	// existing DELETE-sharing reader retains its old image while new readers
	// open the replacement. Ordinary os.Rename uses legacy MoveFileEx, which
	// refuses to replace an open destination even when it shares deletion.
	return root.Rename(filepath.Base(source), filepath.Base(destination))
}

func removeSessionFile(directory, path string) error {
	// #nosec G703 -- directory is the operator-configured session storage root, independent of client workspace IDs.
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	// POSIX disposition makes the name immediately available for another Save
	// while existing DELETE-sharing readers retain their complete old image.
	return root.Remove(filepath.Base(path))
}
