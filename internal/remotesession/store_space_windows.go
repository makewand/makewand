//go:build windows

package remotesession

import "golang.org/x/sys/windows"

// diskFreeBytes returns the bytes the calling user can still use on dir's
// volume, honoring disk quotas.
func diskFreeBytes(dir string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil {
		return 0, err
	}
	return available, nil
}
