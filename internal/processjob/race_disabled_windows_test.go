//go:build windows && !race

package processjob

// raceEnabled reports whether this test binary was built with -race (see
// race_enabled_windows_test.go).
const raceEnabled = false
