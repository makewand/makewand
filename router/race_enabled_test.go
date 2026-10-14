//go:build race

package router

// raceEnabled reports whether this test binary was built with -race. The race
// runtime discards part of what code puts back into a sync.Pool, so the
// regexp package reallocates the matcher state it normally reuses, and it
// slows down every instrumented access: allocation bounds hold only without
// it, and time bounds need more room under it (raceSlowdown).
const raceEnabled = true

// raceSlowdown is the factor by which a time bound of a test grows under
// -race (2-20x is the slowdown the race detector documents).
const raceSlowdown = 5
