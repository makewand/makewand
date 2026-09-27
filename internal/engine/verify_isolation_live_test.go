package engine

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
)

// TestEvaluateCandidateFiles_LiveBwrapIsolation exercises the real bubblewrap
// sandbox end-to-end. It skips when isolation is not available on the host so
// the suite never depends on bwrap being installed — UNLESS MAKEWAND_REQUIRE_BWRAP=1
// (set by CI/release), in which case an unavailable sandbox is a hard failure so
// the security-critical isolation path can never silently go unexercised.
func TestEvaluateCandidateFiles_LiveBwrapIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live sandbox test in short mode")
	}
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		if os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
			t.Fatalf("MAKEWAND_REQUIRE_BWRAP=1 but bubblewrap isolation is unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
		}
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}

	project, err := NewProject("verify-live", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := project.WriteFiles([]ExtractedFile{
		{Path: "go.mod", Content: "module example.com/verifylive\n\ngo 1.22\n"},
		{Path: "math.go", Content: "package verifylive\n\nfunc Add(a, b int) int { return a + b }\n"},
		{Path: "math_test.go", Content: "package verifylive\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"},
	}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := project.ScanFiles(); err != nil {
		t.Fatalf("ScanFiles: %v", err)
	}

	report, verifyErr := project.EvaluateCandidateFiles(context.Background(), []ExtractedFile{{
		Path:    "math.go",
		Content: "package verifylive\n\nfunc Add(a, b int) int { return b + a }\n",
	}})
	if verifyErr != nil {
		t.Fatalf("EvaluateCandidateFiles: %v", verifyErr)
	}
	if !report.Isolated {
		t.Fatalf("report.Isolated = false, want true; report=%+v", report)
	}
	if !report.Passed || report.Strength != 1 {
		t.Fatalf("report = %+v, want isolated local-check pass at strength 1", report)
	}
}

// Bubblewrap configures an isolated loopback interface. A local test server
// remains reachable, while the host listener and default network are hidden.
func TestVerificationNetworkHasOnlyIsolatedLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "0")
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		if os.Getenv("MAKEWAND_REQUIRE_BWRAP") == "1" {
			t.Fatal(RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
		}
		t.Skip("bubblewrap unavailable")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	p, err := NewProject("network", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := `import socket, sys
host_port = int(sys.argv[1])
s = socket.socket()
s.bind(("127.0.0.1",0))
s.listen()
local = socket.create_connection(s.getsockname(), timeout=1)
local.close()
s.close()
try:
    socket.create_connection(("127.0.0.1",host_port), timeout=1)
except OSError:
    pass
else:
    raise RuntimeError("host loopback exposed")
with open("/proc/net/route") as f:
    assert all(line.split()[1] != "00000000" for line in list(f)[1:]), "default route exposed"
`
	result, err := p.RunVerificationPlan(context.Background(), ExecPlan{Kind: "tests", Command: "python3", Args: []string{"-c", source, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)}})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("isolated loopback: %v %+v", err, result)
	}
}
