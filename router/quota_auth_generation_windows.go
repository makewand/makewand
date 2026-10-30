//go:build windows

package router

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func quotaAuthGeneration(path string, info os.FileInfo) ([4]uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return [4]uint64{}, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || info.Size() != opened.Size() || !info.ModTime().Equal(opened.ModTime()) {
		return [4]uint64{}, false
	}
	// FILE_BASIC_INFO includes NTFS ChangeTime, unlike os.FileInfo.Sys().
	// Restoring LastWriteTime cannot conceal an intervening login rewrite.
	var basic struct {
		CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
		FileAttributes                                          uint32
		_                                                       uint32
	}
	handle := windows.Handle(f.Fd())
	if windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))) != nil || basic.ChangeTime <= 0 || basic.CreationTime <= 0 {
		return [4]uint64{}, false
	}
	var id windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(handle, &id) != nil {
		return [4]uint64{}, false
	}
	return [4]uint64{uint64(id.VolumeSerialNumber), uint64(id.FileIndexHigh)<<32 | uint64(id.FileIndexLow),
		uint64(basic.ChangeTime), uint64(basic.CreationTime)}, true
}
