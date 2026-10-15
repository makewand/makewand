//go:build !race

package router

// raceEnabled reports whether this test binary was built with -race (see
// race_enabled_test.go).
const raceEnabled = false

// raceSlowdown is the factor by which a time bound of a test grows under
// -race; 1 without it.
const raceSlowdown = 1
