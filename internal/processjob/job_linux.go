//go:build linux

package processjob

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/internal/trustedbin"
)

// Linux has no Job Object. A memory cap is enforced by starting the command
// inside a transient systemd user scope (cgroup v2 MemoryMax, swap disabled).
// `systemd-run --scope` execs the command in place, so the PID, process group,
// stdio and ExtraFiles descriptors are unchanged for callers. When no user
// manager is reachable the command runs uncapped (MemoryEnforcement reports
// "none"); RLIMIT_AS is never used because it breaks Go/JVM/torch runtimes.

var (
	scopeProbeOnce sync.Once
	scopeTools     struct{ systemdRun, env, nice string }
	scopeAvailable bool

	// lookTrusted and probeScope are test seams.
	lookTrusted = trustedbin.LookPath
	probeScope  = func(systemdRun string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// #nosec G204 -- trusted absolute systemd-run with fixed arguments.
		cmd := exec.CommandContext(ctx, systemdRun, "--user", "--scope", "--collect", "-q",
			"-p", "MemoryMax=64M", "-p", "MemorySwapMax=0", "--", "/bin/true")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
		return cmd.Run() == nil
	}
)

// scopeBusEnv lists variables systemd-run needs to reach the user manager.
var scopeBusEnv = []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}

func memoryScopeTools() (string, string, bool) {
	scopeProbeOnce.Do(func() {
		systemdRun, err := lookTrusted("systemd-run")
		if err != nil {
			return
		}
		env, err := lookTrusted("env")
		if err != nil {
			return
		}
		scopeTools.systemdRun, scopeTools.env = systemdRun, env
		if nice, err := lookTrusted("nice"); err == nil {
			scopeTools.nice = nice // only needed for targets containing '='
		}
		scopeAvailable = probeScope(systemdRun)
	})
	return scopeTools.systemdRun, scopeTools.env, scopeAvailable
}

// MemoryEnforcement reports how Options.MemoryBytes is enforced on this host:
// "systemd-scope" or "none".
func MemoryEnforcement() string {
	if _, _, ok := memoryScopeTools(); ok {
		return "systemd-scope"
	}
	return "none"
}

func envHas(env []string, key string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return true
		}
	}
	return false
}

// wrapMemoryScope rewrites an unstarted command to run under a memory-capped
// scope. Bus variables the child would not otherwise receive are passed to
// systemd-run only and removed again by env(1) before the real command runs.
// env(1) takes every operand containing '=' before the command for a variable
// assignment, so a target path with '=' is not named to it directly: env runs
// `nice -n 0` instead, which execs its operands verbatim and leaves the
// scheduling priority unchanged.
func wrapMemoryScope(cmd *exec.Cmd, memoryBytes uintptr) {
	if memoryBytes == 0 || cmd.Err != nil || cmd.Path == "" {
		return
	}
	target, ok := scopeTarget(cmd)
	if !ok {
		return // let cmd.Start report the missing/non-executable target itself
	}
	systemdRun, envPath, ok := memoryScopeTools()
	if !ok {
		return
	}
	command := []string{target}
	if strings.Contains(target, "=") {
		if scopeTools.nice == "" {
			return // no unambiguous way to name the target to env(1)
		}
		command = []string{scopeTools.nice, "-n", "0", target}
	}
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	wrapperEnv := append([]string(nil), base...)
	var unset []string
	for _, key := range scopeBusEnv {
		if envHas(base, key) {
			continue
		}
		if value, present := os.LookupEnv(key); present {
			wrapperEnv = append(wrapperEnv, key+"="+value)
			unset = append(unset, "-u", key)
		}
	}
	if !envHas(base, "INVOCATION_ID") {
		unset = append(unset, "-u", "INVOCATION_ID") // set by systemd-run itself
	}
	args := []string{systemdRun, "--user", "--scope", "--collect", "-q",
		"-p", "MemoryMax=" + strconv.FormatUint(uint64(memoryBytes), 10),
		"-p", "MemorySwapMax=0", "--", envPath}
	args = append(args, unset...)
	args = append(args, "--")
	args = append(args, command...)
	if len(cmd.Args) > 1 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Path = systemdRun
	cmd.Args = args
	cmd.Env = wrapperEnv
}

// scopeTarget resolves the command the scope will exec. Wrapping a missing or
// non-executable target would turn a synchronous Start error into a later exit
// status, so such commands are left unwrapped for cmd.Start to reject.
func scopeTarget(cmd *exec.Cmd) (string, bool) {
	target := cmd.Path
	if !filepath.IsAbs(target) {
		base := cmd.Dir
		if base == "" {
			wd, err := os.Getwd()
			if err != nil {
				return "", false
			}
			base = wd
		} else if !filepath.IsAbs(base) {
			wd, err := os.Getwd()
			if err != nil {
				return "", false
			}
			base = filepath.Join(wd, base)
		}
		target = filepath.Join(base, target)
	}
	if !filepath.IsAbs(target) || strings.Contains(target, "..") {
		return "", false
	}
	info, err := os.Stat(target) //nolint:gosec // G703: stat of the caller-chosen executable, never opened
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return target, true
}

func start(cmd *exec.Cmd, options Options) (func(), error) {
	if err := prepare(cmd); err != nil {
		return nil, err
	}
	wrapMemoryScope(cmd, options.MemoryBytes)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Linux callers retain their existing process-group or sandbox policies.
	return func() {}, nil
}

// Kill exists for shared call sites. Unix process-group cancellation remains
// in the platform helpers, because a plain process is not a process group.
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
