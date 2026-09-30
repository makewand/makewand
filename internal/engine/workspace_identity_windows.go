//go:build windows

package engine

import (
	"fmt"
	"math"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func checkpointIdentity(path string, info os.FileInfo) (checkpointFileIdentity, error) {
	if !info.Mode().IsRegular() {
		return checkpointFileIdentity{}, fmt.Errorf("checkpoint input is not a regular file")
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return checkpointFileIdentity{}, err
	}
	// Query one fixed handle, without following a final reparse point. Share
	// deletion so collecting identity cannot impede subsequent atomic replace.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return checkpointFileIdentity{}, fmt.Errorf("open checkpoint identity: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var file windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &file); err != nil {
		return checkpointFileIdentity{}, fmt.Errorf("get checkpoint identity: %w", err)
	}
	index := uint64(file.FileIndexHigh)<<32 | uint64(file.FileIndexLow)
	if file.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || file.NumberOfLinks == 0 || index == 0 || file.VolumeSerialNumber == 0 {
		return checkpointFileIdentity{}, fmt.Errorf("regular file identity is unavailable")
	}
	// BY_HANDLE_FILE_INFORMATION's 64-bit file index is not guaranteed unique
	// on ReFS. Accept the documented NTFS/FAT identity formats and fail closed
	// on filesystems whose identity guarantees have not been established.
	var filesystem [32]uint16
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return checkpointFileIdentity{}, fmt.Errorf("get checkpoint filesystem: %w", err)
	}
	switch strings.ToUpper(windows.UTF16ToString(filesystem[:])) {
	case "NTFS", "FAT", "FAT32", "EXFAT":
	default:
		return checkpointFileIdentity{}, fmt.Errorf("checkpoint filesystem does not provide an established 64-bit file identity")
	}
	size := uint64(file.FileSizeHigh)<<32 | uint64(file.FileSizeLow)
	if size > math.MaxInt64 {
		return checkpointFileIdentity{}, fmt.Errorf("checkpoint file size is out of range")
	}
	if int64(size) != info.Size() || file.LastWriteTime.Nanoseconds() != info.ModTime().UnixNano() {
		return checkpointFileIdentity{}, fmt.Errorf("checkpoint input changed while opening identity")
	}
	return checkpointFileIdentity{Nlink: uint64(file.NumberOfLinks), Dev: uint64(file.VolumeSerialNumber), Ino: index}, nil
}
