//go:build !linux

package engine

import (
	"errors"
	"os"
)

func tryReflink(*os.File, *os.File) error { return errors.New("reflink unavailable on this platform") }

func reflinkUnsupported(err error) bool              { return err != nil }
func workspaceCopyDevice(os.FileInfo) (uint64, bool) { return 0, true }
func workspaceReflinkFilesystem(string, string) bool { return false }
