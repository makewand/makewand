//go:build windows

package execution

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/makewand/makewand/internal/privatefile"
	"golang.org/x/sys/windows"
)

// Root.OpenFile already shares READ, WRITE and DELETE in Go 1.26. Keep its
// handle-relative, reparse-safe open instead of reopening a pathname with
// CreateFile. Legacy readers and scanners can still briefly deny deletion.
func retryAccountingIO(operation func() error) error {
	for attempt := 0; ; attempt++ {
		err := operation()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		if attempt == 3 {
			return fmt.Errorf("Windows accounting I/O sharing violation after 4 attempts: %w", err)
		}
		time.Sleep((5 * time.Millisecond) << attempt)
	}
}

func protectExecutionFile(file *os.File) error {
	// Do not rewrite a protected DACL on every lock/read. Besides extra I/O,
	// SetSecurityInfo can trigger filesystem filters during capacity polling.
	// Validate on every open, so permissions/owner/link changes still fail or
	// pass through Tighten's authorization checks on this same fixed object.
	if err := privatefile.Validate(file); err == nil {
		return nil
	}
	return privatefile.Tighten(file)
}
