//go:build windows

package remotesession

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func readSessionFile(path string) ([]byte, error) {
	file, err := openSessionRead(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func openSessionRead(path string) (*os.File, error) {
	name := path
	// Match os.Open's support for long paths, including UNC paths, without
	// changing Win32 normalization for ordinary short filenames.
	if len(name) >= 248 && !strings.HasPrefix(name, `\\?\`) && !strings.HasPrefix(name, `\\.\`) {
		absolute, err := filepath.Abs(name)
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		if strings.HasPrefix(absolute, `\\`) {
			name = `\\?\UNC\` + absolute[2:]
		} else {
			name = `\\?\` + absolute
		}
	}
	encoded, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// os.Open omits FILE_SHARE_DELETE. A reader must permit replacement and
	// deletion by another Save/Delete, while continuing to read its old image.
	handle, err := windows.CreateFile(encoded, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
