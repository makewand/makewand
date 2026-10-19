//go:build !windows

package execution

import (
	"os"

	"github.com/makewand/makewand/internal/privatefile"
)

func retryAccountingIO(operation func() error) error { return operation() }

func protectExecutionFile(file *os.File) error { return privatefile.Tighten(file) }
