//go:build !windows

package engine

import "os"

func privatePendingFile(file *os.File) error { return file.Chmod(0o600) }
