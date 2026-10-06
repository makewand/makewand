//go:build windows

package backup

import (
	"strings"

	"golang.org/x/sys/windows"
)

// Direct Win32 opens need the extended representation that Go's os package
// supplies for long paths. Keep business paths unchanged and preserve existing
// device/extended namespace inputs; this function grants no path authority.
func restoreWindowsName(path string) (*uint16, error) {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) || strings.HasPrefix(path, `\\.\`) {
		return windows.UTF16PtrFromString(path)
	}
	full, err := windows.FullPath(path)
	if err != nil {
		return nil, err
	}
	if len(full) < 248 {
		return windows.UTF16PtrFromString(path)
	}
	if strings.HasPrefix(full, `\\`) {
		full = `\\?\UNC\` + full[2:]
	} else {
		full = `\\?\` + full
	}
	return windows.UTF16PtrFromString(full)
}
