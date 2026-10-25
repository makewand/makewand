package engine

import (
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/makewand/makewand/internal/seccompbpf"
)

// pip's binary-only policy does not cover a source URL nested in a fetched
// wheel's Requires-Dist. Keep pip's wheel resolver, but irrevocably deny new
// executables after trusted startup probes and before pip reads requirements.
// The audit hook supplies a nonce-bound diagnostic; the OS filter enforces
// the execution boundary independently of Python hooks.
const pipFetchGuardScript = `import base64, ctypes, os, runpy, struct, sys
tag = sys.argv[2]
def refused():
    sys.stderr.write("\n" + tag + "\n")
    sys.stderr.flush()
try:
    if sys.platform != "linux" or struct.calcsize("P") != 8 or sys.byteorder != "little":
        raise RuntimeError("unsupported pip execution guard platform")
    # Cache pip's trusted OS/tag probes before denying subprocesses. They
    # receive no project requirements or metadata; PATH excludes workspace.
    from pip._internal.network.session import user_agent
    from pip._internal.utils.compatibility_tags import get_supported
    user_agent()
    get_supported()
    class Instruction(ctypes.Structure):
        _fields_ = [("code", ctypes.c_ushort), ("jt", ctypes.c_ubyte), ("jf", ctypes.c_ubyte), ("k", ctypes.c_uint32)]
    class Program(ctypes.Structure):
        _fields_ = [("length", ctypes.c_ushort), ("filter", ctypes.POINTER(Instruction))]
    raw = base64.b64decode(sys.argv[1], validate=True)
    if not raw or len(raw) % ctypes.sizeof(Instruction):
        raise RuntimeError("invalid pip execution guard")
    instructions = (Instruction * (len(raw) // ctypes.sizeof(Instruction))).from_buffer_copy(raw)
    program = Program(len(instructions), instructions)
    libc = ctypes.CDLL(None, use_errno=True)
    libc.prctl.argtypes = [ctypes.c_int, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong]
    libc.prctl.restype = ctypes.c_int
    if libc.prctl(38, 1, 0, 0, 0) or libc.prctl(22, 2, ctypes.addressof(program), 0, 0):
        raise OSError(ctypes.get_errno(), "cannot install pip execution guard")
except Exception:
    refused()
    raise SystemExit(70)
def audit(event, args):
    if event in ("subprocess.Popen", "os.system", "os.posix_spawn", "os.exec", "os.fork", "os.forkpty"):
        refused()
        raise RuntimeError("dependency metadata cannot start a process during a networked fetch")
sys.addaudithook(audit)
entry = sys.argv[3]
sys.argv = [entry] + sys.argv[4:]
runpy.run_path(entry, run_name="__main__")
`

func guardedPipFetch(workspace, name string, args []string) (command string, guarded []string, tag, refusal string) {
	filter := seccompbpf.GenerateExecGuardFilter()
	entry := fetchToolPath(name, workspace)
	python := scriptInterpreterAny(entry, fetchPathEnv(workspace))
	if runtime.GOOS != "linux" || len(filter) == 0 || !filepath.IsAbs(entry) || !filepath.IsAbs(python) ||
		!strings.HasPrefix(strings.ToLower(filepath.Base(python)), "python") {
		return "", nil, "", "a safe pip download requires a trusted Python outside the workspace and a Linux amd64/arm64 execution guard"
	}
	tag = "MAKEWAND_PIP_FETCH_CODE_REFUSED_" + rand.Text()
	guarded = []string{"-I", "-c", pipFetchGuardScript, base64.StdEncoding.EncodeToString(filter), tag, entry}
	guarded = append(guarded, args...)
	return python, guarded, tag, ""
}
