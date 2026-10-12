package processjob

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// MemoryLimitEnv overrides the default sandbox memory cap. It accepts a byte
// count or a size with a binary suffix (K, M, G, T, optionally followed by
// "i", "iB" or "B", case-insensitive; all are powers of 1024). "0", "off",
// "none", "unlimited" and "infinity" disable the cap.
const MemoryLimitEnv = "MAKEWAND_SANDBOX_MEMORY_MAX"

// DefaultMemoryBytes is the per-command memory cap when MemoryLimitEnv is unset.
const DefaultMemoryBytes uint64 = 8 << 30

// DefaultMaxProcesses is the simultaneous-process cap Start applies on Windows.
const DefaultMaxProcesses = 256

// ParseMemorySize parses a MemoryLimitEnv value. Zero means unlimited.
func ParseMemorySize(value string) (uint64, error) {
	s := strings.ToLower(strings.TrimSpace(value))
	switch s {
	case "0", "off", "none", "unlimited", "infinity":
		return 0, nil
	case "":
		return 0, fmt.Errorf("empty memory size")
	}
	s = strings.TrimSuffix(s, "b")
	s = strings.TrimSuffix(s, "i")
	shift := 0
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'k':
			shift = 10
		case 'm':
			shift = 20
		case 'g':
			shift = 30
		case 't':
			shift = 40
		}
		if shift > 0 {
			s = s[:n-1]
		}
	}
	number, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q", value)
	}
	if number > math.MaxUint64>>shift {
		return 0, fmt.Errorf("memory size %q overflows", value)
	}
	bytes := number << shift
	if bytes != 0 && bytes < 16<<20 {
		return 0, fmt.Errorf("memory size %q is below the 16M minimum", value)
	}
	return bytes, nil
}

// SandboxMemoryBytes returns the configured cap (DefaultMemoryBytes unless
// MemoryLimitEnv overrides it). An invalid value is an error so callers fail
// closed instead of silently running uncapped.
func SandboxMemoryBytes() (uint64, error) {
	value, ok := os.LookupEnv(MemoryLimitEnv)
	if !ok || strings.TrimSpace(value) == "" {
		return DefaultMemoryBytes, nil
	}
	bytes, err := ParseMemorySize(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", MemoryLimitEnv, err)
	}
	return bytes, nil
}

// SandboxOptions are the limits for untrusted or AI-driven subprocesses:
// DefaultMaxProcesses plus the SandboxMemoryBytes cap.
//
// Enforcement: Windows applies JOB_OBJECT_LIMIT_JOB_MEMORY to the whole Job.
// Linux runs the command in a transient `systemd-run --user --scope` unit with
// MemoryMax/MemorySwapMax=0 when a user systemd manager is reachable (see
// MemoryEnforcement); otherwise, and on other Unix systems, the cap is not
// enforced. RLIMIT_AS is deliberately not used: it breaks runtimes that reserve
// large virtual ranges (Go, JVMs, torch/BLAS).
func SandboxOptions() (Options, error) {
	bytes, err := SandboxMemoryBytes()
	if err != nil {
		return Options{}, err
	}
	if bytes > math.MaxUint {
		bytes = math.MaxUint
	}
	return Options{MaxProcesses: DefaultMaxProcesses, MemoryBytes: uintptr(bytes)}, nil
}

// StartSandboxed is StartWithOptions(cmd, SandboxOptions()).
func StartSandboxed(cmd *exec.Cmd) (func(), error) {
	options, err := SandboxOptions()
	if err != nil {
		return nil, err
	}
	return start(cmd, options)
}
