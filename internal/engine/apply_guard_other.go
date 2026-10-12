//go:build !linux

package engine

import "os"

// Non-Linux platforms use verify-then-rename (see guardedReplaceApplyFile).
func platformApplyRenameExchange(*os.Root, string, string) error { return errApplyAtomicUnsupported }

func platformApplyRenameNoReplace(*os.Root, string, string) error { return errApplyAtomicUnsupported }
