//go:build windows

package main

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
)

func openUnsafeHostExecAudit(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(absolute, `\\?\`) || strings.HasPrefix(absolute, `\\.\`) {
		return nil, fmt.Errorf("audit path must use an ordinary filesystem namespace")
	}
	apiPath := absolute
	volume := filepath.VolumeName(absolute)
	if len(volume) == 2 && volume[1] == ':' {
		apiPath = `\\?\` + absolute
	} else if strings.HasPrefix(volume, `\\`) {
		apiPath = `\\?\UNC\` + strings.TrimPrefix(absolute, `\\`)
	}
	name, err := windows.UTF16PtrFromString(apiPath)
	if err != nil {
		return nil, err
	}
	// APPEND_DATA keeps every Write at EOF; OPEN_REPARSE_POINT lets Tighten reject
	// the leaf itself before any outside referent's permissions or bytes change.
	handle, err := windows.CreateFile(name, windows.FILE_APPEND_DATA|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("audit file handle unavailable")
	}
	return file, nil
}
