//go:build !unix && !windows

package main

import (
	"fmt"
	"os"
)

func openUnsafeHostExecAudit(string) (*os.File, error) {
	return nil, fmt.Errorf("private audit files are unsupported on this platform")
}
