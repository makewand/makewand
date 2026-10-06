// Package testfixture supplies controlled, portable CLI children to Go tests.
// Production packages must not import it. Each fixture is the actual test
// executable copied beside an explicit, parent-created JSON specification.
package testfixture

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const ArgSeparator = "\n----ARG----\n"

// A copied fixture remains identifiable even if its sidecar is hidden by a
// sandbox mount or removed. Missing evidence must never re-enter the test suite.
const executableMarker = "MAKEWAND_OWNED_TEST_FIXTURE_V1\x00"

// Spec selects fixed local behavior; it never starts a shell, network client or
// real model. Paths refer only to the parent's owned fixture directory.
type Spec struct {
	Mode       string
	Stdout     string
	Stderr     string
	ExitCode   int
	Sleep      time.Duration
	Sentinel   string
	StateFile  string
	FirstError string
	LogDir     string
}

// WriteCLI creates a native executable and its controlled sidecar. It works
// before any TestMain because the child is intercepted during package init.
func WriteCLI(t testing.TB, name string, spec Spec) string {
	t.Helper()
	return WriteCLIAt(t, t.TempDir(), name, spec)
}

// WriteCLIAt additionally permits discovery tests to place several executable
// names on one isolated PATH. No source or system toolchain is compiled here.
func WriteCLIAt(t testing.TB, dir, name string, spec Spec) string {
	t.Helper()
	if filepath.Base(name) != name || name == "." || name == "" {
		t.Fatal("invalid fixture executable name")
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(name), ".exe") {
		name += ".exe"
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	path := filepath.Join(dir, name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if _, err := io.WriteString(out, executableMarker); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".fixture.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	// The race runtime normally sleeps a second on each successful child exit.
	// This only removes that instrumentation delay from owned fixture children.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return path
}

// ClearProviderEnv makes a test's synthetic policy independent of the native
// gate's disabled provider environment. It never probes or enables a real CLI.
func ClearProviderEnv(t testing.TB) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "MAKEWAND_ENABLE_") || strings.HasPrefix(upper, "MAKEWAND_DISABLE_") {
			t.Setenv(key, "")
		}
	}
}

func init() {
	executable, err := os.Executable()
	if err != nil {
		return
	}
	self, err := os.Open(executable)
	if err != nil {
		return
	}
	selfInfo, err := self.Stat()
	if err != nil || selfInfo.Size() < int64(len(executableMarker)) {
		self.Close()
		return
	}
	marker := make([]byte, len(executableMarker))
	_, markerErr := self.ReadAt(marker, selfInfo.Size()-int64(len(marker)))
	self.Close()
	if markerErr != nil || string(marker) != executableMarker {
		return
	}
	sidecar := executable + ".fixture.json"
	info, err := os.Lstat(sidecar)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		os.Exit(90)
	}
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		os.Exit(90)
	}
	var spec Spec
	if json.Unmarshal(raw, &spec) != nil {
		os.Exit(90)
	}
	os.Exit(run(spec, os.Args[1:]))
}

func run(spec Spec, args []string) int {
	if spec.Sleep > 0 {
		time.Sleep(spec.Sleep)
	}
	if spec.Sentinel != "" {
		if err := os.WriteFile(spec.Sentinel, []byte("called\n"), 0600); err != nil {
			return 91
		}
	}
	switch spec.Mode {
	case "", "output":
		fmt.Fprint(os.Stdout, spec.Stdout)
		fmt.Fprint(os.Stderr, spec.Stderr)
		return spec.ExitCode
	case "echo-args":
		for _, arg := range args {
			fmt.Fprintln(os.Stdout, arg)
		}
		return spec.ExitCode
	case "echo-stdin":
		first := ""
		if len(args) > 0 {
			first = args[0]
		}
		fmt.Fprintf(os.Stdout, "mode:%s\n", first)
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 92
		}
		return spec.ExitCode
	case "probe":
		if len(args) > 0 && args[0] == "--version" {
			fmt.Fprint(os.Stdout, spec.Stdout)
			return spec.ExitCode
		}
		return 1
	case "agy-prompt":
		if len(args) != 3 || args[0] != "--sandbox" || args[1] != "--print" {
			fmt.Fprint(os.Stderr, "unexpected controlled argv\n")
			return 64
		}
		fmt.Fprintln(os.Stdout, args[2])
		return 0
	case "counter":
		state := spec.StateFile
		if state == "" && len(args) > 0 {
			state = args[0]
		}
		if state == "" {
			return 93
		}
		n := 0
		if b, err := os.ReadFile(state); err == nil { // #nosec G703 -- controlled test-owned counter path, never external input.
			n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		n++
		if err := os.WriteFile(state, []byte(strconv.Itoa(n)+"\n"), 0600); err != nil { // #nosec G703 -- controlled test-owned counter path.
			return 93
		}
		if n == 1 && spec.FirstError != "" {
			fmt.Fprintln(os.Stderr, spec.FirstError)
			return 1
		}
		fmt.Fprint(os.Stdout, spec.Stdout)
		fmt.Fprint(os.Stderr, spec.Stderr)
		return spec.ExitCode
	case "record-codex":
		if len(args) > 0 && args[0] == "--version" {
			fmt.Fprintln(os.Stdout, "codex-fake")
			return 0
		}
		file, err := os.CreateTemp(spec.LogDir, "call.")
		if err != nil {
			return 94
		}
		path := file.Name()
		if file.Close() != nil {
			return 94
		}
		dir, err := os.Getwd()
		if err != nil {
			return 94
		}
		if real, e := filepath.EvalSymlinks(dir); e == nil {
			dir = real
		}
		if os.WriteFile(path+".dir", []byte(dir+"\n"), 0600) != nil {
			return 94
		}
		if os.WriteFile(path+".args", []byte(strings.Join(args, ArgSeparator)+ArgSeparator), 0600) != nil { // #nosec G703 -- path is the fixture-owned CreateTemp result.
			return 94
		}
		if os.Remove(path) != nil {
			return 94
		}
		fmt.Fprintln(os.Stdout, `{"type":"item.completed","item":{"type":"agent_message","text":"FAKE-CODEX-ANSWER"}}`)
		return 0
	default:
		return 95
	}
}
