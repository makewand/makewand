//go:build windows

package fsperm

// UntraversableForeignDir is never true on Windows; scanners fail closed.
func UntraversableForeignDir(string) bool { return false }
