package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/makewand/makewand/internal/seccompbpf"
)

func mustWrapVerification(t *testing.T, bwrapPath, workspace, command string, args []string, allowNetwork bool, readOnly []string) (string, []string) {
	t.Helper()
	cmd, wrapped, err := wrapVerificationCommand(bwrapPath, workspace, command, args, allowNetwork, readOnly, false, false)
	if err != nil {
		t.Fatalf("wrapVerificationCommand: %v", err)
	}
	return cmd, wrapped
}

func swapSeccompVars(t *testing.T, goos string, supported bool, env map[string]string) *[]string {
	t.Helper()
	oldGOOS, oldSupported, oldGetenv, oldWarn := sandboxSeccompGOOS, sandboxSeccompSupported, sandboxSeccompGetenv, sandboxSeccompWarn
	var warnings []string
	sandboxSeccompGOOS = goos
	sandboxSeccompSupported = func() bool { return supported }
	sandboxSeccompGetenv = func(key string) string { return env[key] }
	sandboxSeccompWarn = func(msg string) { warnings = append(warnings, msg) }
	sandboxSeccompWarnOnce = &sync.Once{}
	t.Cleanup(func() {
		sandboxSeccompGOOS, sandboxSeccompSupported, sandboxSeccompGetenv, sandboxSeccompWarn = oldGOOS, oldSupported, oldGetenv, oldWarn
		sandboxSeccompWarnOnce = &sync.Once{}
	})
	return &warnings
}

func TestEngineBwrapBuildersRequireSeccompByDefault(t *testing.T) {
	swapSeccompVars(t, "linux", true, nil)
	fakeWorkingBwrap(t)
	_, args := mustWrapVerification(t, "/usr/bin/bwrap", t.TempDir(), "go", []string{"test"}, false, nil)
	if !sandboxSeccompRequested(args) {
		t.Fatalf("verification argv lacks leading --seccomp 3: %v", args)
	}
	fakePreviewSandbox(t)
	_, previewArgs, err := wrapPreviewProjectCommand(t.TempDir(), "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{})
	if err != nil {
		t.Fatal(err)
	}
	if !sandboxSeccompRequested(previewArgs) {
		t.Fatalf("preview argv lacks leading --seccomp 3: %v", previewArgs)
	}
	policy, err := freezeTrustedAcceptance(stdioAcceptanceSpec(), "/opt/private/acceptance.json")
	if err != nil {
		t.Fatal(err)
	}
	_, acceptanceArgs, err := wrapTrustedAcceptanceCommand("/usr/bin/bwrap", t.TempDir(), policy)
	if err != nil || !sandboxSeccompRequested(acceptanceArgs) {
		t.Fatalf("trusted acceptance argv lacks leading --seccomp 3: %v %v", acceptanceArgs, err)
	}
}

func TestEngineSeccompOptOutWarns(t *testing.T) {
	warnings := swapSeccompVars(t, "linux", true, map[string]string{"MAKEWAND_ALLOW_NO_SECCOMP": "1"})
	fakeWorkingBwrap(t)
	_, args := mustWrapVerification(t, "/usr/bin/bwrap", t.TempDir(), "go", []string{"test"}, false, nil)
	if containsArg(args, "--seccomp") {
		t.Fatalf("opt-out still requested seccomp: %v", args)
	}
	if len(*warnings) != 1 || !strings.Contains((*warnings)[0], "MAKEWAND_ALLOW_NO_SECCOMP") {
		t.Fatalf("opt-out warning = %v", *warnings)
	}
}

func TestEngineSeccompUnsupportedArchAndNonLinux(t *testing.T) {
	warnings := swapSeccompVars(t, "linux", false, nil)
	if got := sandboxSeccompArgs(); got != nil || len(*warnings) != 1 {
		t.Fatalf("unsupported arch: args=%v warnings=%v", got, *warnings)
	}
	swapSeccompVars(t, "darwin", true, nil)
	if got := sandboxSeccompArgs(); got != nil {
		t.Fatalf("non-linux args = %v", got)
	}
}

func TestAttachSandboxSeccompSuppliesFilterAsFD3(t *testing.T) {
	if !seccompbpf.Supported() {
		t.Skip("no filter for this architecture")
	}
	cmd := exec.Command("/bin/true")
	closeFn, err := attachSandboxSeccomp(cmd, []string{"--seccomp", "3", "--die-with-parent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 1 {
		t.Fatalf("ExtraFiles = %d", len(cmd.ExtraFiles))
	}
	data, err := io.ReadAll(cmd.ExtraFiles[0])
	closeFn()
	closeFn() // idempotent
	if err != nil || string(data) != string(seccompbpf.GenerateFilter()) {
		t.Fatalf("filter pipe content mismatch: %v", err)
	}
	other := exec.Command("/bin/true")
	if closeFn, err := attachSandboxSeccomp(other, []string{"--die-with-parent"}); err != nil || len(other.ExtraFiles) != 0 {
		t.Fatalf("unrequested filter attached: %v", err)
	} else {
		closeFn()
	}
	busy := exec.Command("/bin/true")
	busy.ExtraFiles = []*os.File{os.Stdin}
	if _, err := attachSandboxSeccomp(busy, []string{"--seccomp", "3"}); err == nil {
		t.Fatal("filter attached at the wrong descriptor")
	}
}

// Live: the real bubblewrap launch installs the filter (Seccomp: 2 in status).
func TestVerificationSandboxLiveSeccompActive(t *testing.T) {
	if testing.Short() || runtime.GOOS != "linux" || !seccompbpf.Supported() {
		t.Skip("live linux seccomp test")
	}
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	t.Setenv("MAKEWAND_ALLOW_NO_SECCOMP", "")
	env, err := resolveVerifyExecEnvironment(UnsafeHostExecAuthorization{})
	if err != nil || env.mode != verifyExecIsolated {
		t.Skipf("bubblewrap isolation unavailable: %v", err)
	}
	workspace := t.TempDir()
	p := &Project{Path: workspace}
	command, args, err := wrapVerificationCommand(env.bwrapPath, workspace, "/bin/sh", []string{"-c", "grep '^Seccomp:' /proc/self/status"}, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.execWithPolicy(context.Background(), command, args, isolatedExecPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "Seccomp:\t2") {
		t.Fatalf("sandboxed process is not seccomp-filtered: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func makeUnixSocketFile(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Skipf("cannot bind unix socket: %v", err)
	}
}

func TestSocketMaskTruncationFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	ws := t.TempDir()
	for i := 0; i < maxSandboxSocketMasks; i++ {
		makeUnixSocketFile(t, filepath.Join(ws, "s"+strconv.Itoa(i)))
	}
	masks, err := sandboxWorkspaceSocketMasks(ws)
	if err != nil || len(masks) != 3*maxSandboxSocketMasks {
		t.Fatalf("exactly %d sockets: masks=%d err=%v", maxSandboxSocketMasks, len(masks)/3, err)
	}
	makeUnixSocketFile(t, filepath.Join(ws, "overflow"))
	if _, err := sandboxWorkspaceSocketMasks(ws); !errors.Is(err, ErrSandboxSocketScan) {
		t.Fatalf("truncated scan not reported: %v", err)
	}
	fakeWorkingBwrap(t)
	if _, _, err := wrapVerificationCommand("/usr/bin/bwrap", ws, "go", []string{"test"}, false, nil, false, false); !errors.Is(err, ErrSandboxSocketScan) {
		t.Fatalf("verification builder ignored truncation: %v", err)
	}
	fakePreviewSandbox(t)
	if _, _, err := wrapPreviewProjectCommand(ws, "npm", []string{"run", "dev"}, UnsafeHostExecAuthorization{}); !errors.Is(err, ErrSandboxSocketScan) {
		t.Fatalf("preview builder ignored truncation: %v", err)
	}
}

func TestSocketMaskUnreadableDirectoryFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs non-root POSIX permissions")
	}
	ws := t.TempDir()
	hidden := filepath.Join(ws, "hidden")
	if err := os.Mkdir(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	makeUnixSocketFile(t, filepath.Join(hidden, "daemon.sock"))
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o700) })
	if _, err := sandboxWorkspaceSocketMasks(ws); !errors.Is(err, ErrSandboxSocketScan) {
		t.Fatalf("unreadable directory hid a socket silently: %v", err)
	}
}

// An unreadable directory the current user owns could be chmod-ed back from
// inside the sandbox, so it keeps the scan incomplete (fail closed).
func TestSocketMaskOwnUnreadableDirFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permissions as a non-root user")
	}
	ws := t.TempDir()
	locked := filepath.Join(ws, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := sandboxWorkspaceSocketMasks(ws); !errors.Is(err, ErrSandboxSocketScan) {
		t.Fatalf("own unreadable directory did not fail closed: %v", err)
	}
}
