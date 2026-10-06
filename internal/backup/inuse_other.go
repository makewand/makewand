//go:build !unix && !windows

package backup

// stateDBInUse cannot probe SQLite's POSIX locks on this platform; callers
// rely on the documented "stop the server first" precondition.
func stateDBInUse(string) (bool, error) { return false, nil }
