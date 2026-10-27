//go:build !windows

package router

func isSharingViolation(error) bool { return false }
