//go:build !linux && !darwin && !freebsd && !windows

package remotesession

func diskFreeBytes(string) (uint64, error) { return 0, errFreeSpaceUnsupported }
