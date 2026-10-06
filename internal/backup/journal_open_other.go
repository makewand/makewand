//go:build !windows && !unix

package backup

import "os"

func openRestoreJournal(path string) (*os.File, error) { return os.Open(path) }
