package router

import (
	"os"
	"path/filepath"
)

// The stable sidecar is never renamed: locking users.json itself would lock an
// obsolete inode after the atomic replacement and let another writer proceed.
func (us *UserStore) lockUserFile(exclusive bool) (func(), error) {
	if err := us.ensureDataDir(); err != nil {
		return nil, err
	}
	// #nosec G703 -- this fixed sidecar is under the operator-configured data directory; requests cannot choose its path.
	f, err := os.OpenFile(us.usersFilePath()+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockUserHandle(f, exclusive); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = unlockUserHandle(f); _ = f.Close() }, nil
}

func replaceUserFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".users-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// #nosec G703 -- the temporary file is created in the configured directory and path is its fixed users.json destination.
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncUserDirectory(filepath.Dir(path))
}
