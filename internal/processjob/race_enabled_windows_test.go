//go:build windows && race

package processjob

// raceEnabled reports whether this test binary was built with -race. A race
// instrumented child commits far more memory at startup, so job memory limits
// sized for a plain child can kill it before a test inspects its job.
const raceEnabled = true
