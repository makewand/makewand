"""Versioned daemon request validation and process cancellation helpers."""

import math
import os
import re
import signal
import subprocess
import time
from pathlib import Path

PROTOCOL_VERSION = 1
MAX_REQUEST_BYTES = 1024 * 1024
MAX_MESSAGE_BYTES = 65536
MAX_OUTPUT_BYTES = 16 * 1024 * 1024
DEFAULT_TIMEOUT = 600.0
MAX_TIMEOUT = 3600.0
REQUEST_READ_TIMEOUT = 5.0
SEND_TIMEOUT = 2.0
OUTPUT_CHUNK_BYTES = 8192
EXIT_IPC_ERROR = 70
EXIT_BUSY = 75
EXIT_TIMEOUT = 124
EXIT_CANCELLED = 130


def validate_request(request):
    if not isinstance(request, dict):
        raise ValueError("request must be an object")
    if request.get("version") != PROTOCOL_VERSION:
        raise ValueError("unsupported daemon protocol; restart daemon after upgrading")
    request_id = request.get("request_id")
    if not isinstance(request_id, str) or not re.fullmatch(r"[0-9a-f]{32}", request_id):
        raise ValueError("request_id must be a UUID hex string")
    if request.get("cmd") != "execute":
        raise ValueError("unsupported execution command")
    argv = request.get("argv")
    if not isinstance(argv, list) or not argv or len(argv) > 256:
        raise ValueError("argv must contain between 1 and 256 arguments")
    if any(not isinstance(arg, str) or "\0" in arg for arg in argv):
        raise ValueError("argv must contain strings without NUL bytes")
    cwd = request.get("cwd")
    if not isinstance(cwd, str) or "\0" in cwd or not os.path.isabs(cwd) or not os.path.isdir(cwd):
        raise ValueError("cwd must be an existing absolute invocation directory")
    env = request.get("env")
    if not isinstance(env, dict) or len(env) > 1024:
        raise ValueError("env must be the client's complete environment snapshot")
    for key, value in env.items():
        if (not isinstance(key, str) or not key or "=" in key or "\0" in key
                or not isinstance(value, str) or "\0" in value):
            raise ValueError("invalid environment entry")
    stdin = request.get("stdin", "")
    if not isinstance(stdin, str):
        raise ValueError("stdin must be text")
    timeout = request.get("timeout", DEFAULT_TIMEOUT)
    if (isinstance(timeout, bool) or not isinstance(timeout, (int, float))
            or not math.isfinite(timeout) or not 0 < timeout <= MAX_TIMEOUT):
        raise ValueError(f"timeout must be between 0 and {MAX_TIMEOUT:g} seconds")
    return request


def worker_environment(client_env):
    """Never inherit a resident process's old policies, paths or credentials."""
    env = dict(client_env)
    env.pop("MAKEWAND_DAEMON", None)
    env.pop("MAKEWAND_DISPATCH_ID", None)
    env.update(MAKEWAND_INSIDE_DAEMON="1", MAKEWAND_NO_DAEMON="1",
               PYTHONUNBUFFERED="1", PYTHONDONTWRITEBYTECODE="1")
    return env


def _process_tree(root_pid, marker=None, include_root=True):
    """Snapshot descendants, including provider processes that call setsid()."""
    records = {}
    proc = Path("/proc")
    if proc.is_dir():
        for entry in proc.iterdir():
            if not entry.name.isdigit():
                continue
            try:
                raw = (entry / "stat").read_text()
                fields = raw[raw.rindex(")") + 2:].split()
                records[int(entry.name)] = (int(fields[1]), fields[19])
            except (OSError, ValueError, IndexError):
                continue
    else:
        try:
            result = subprocess.run(["ps", "-axo", "pid=,ppid="], capture_output=True,
                                    text=True, timeout=0.5)
            for line in result.stdout.splitlines():
                pid, parent = map(int, line.split())
                records[pid] = (parent, None)
        except (OSError, ValueError, subprocess.TimeoutExpired):
            pass
    descendants = {root_pid: records[root_pid][1]} if include_root and root_pid in records else {}
    if marker and proc.is_dir():
        wanted = f"MAKEWAND_DAEMON_REQUEST_ID={marker}".encode()
        for pid, (_, token) in records.items():
            try:
                if wanted in (proc / str(pid) / "environ").read_bytes().split(b"\0"):
                    descendants[pid] = token
            except OSError:
                pass
    while True:
        added = {pid: token for pid, (parent, token) in records.items()
                 if parent in descendants and pid not in descendants}
        if not added:
            return descendants
        descendants.update(added)


def _signal_owned(pid, token, signum):
    # A descendant can exit and its PID can be reused during the grace period.
    if token is not None:
        try:
            raw = Path(f"/proc/{pid}/stat").read_text()
            if raw[raw.rindex(")") + 2:].split()[19] != token:
                return
        except (OSError, ValueError, IndexError):
            return
    try:
        os.kill(pid, signum)
    except ProcessLookupError:
        pass


def _is_alive(pid, token):
    if token is not None:
        try:
            raw = Path(f"/proc/{pid}/stat").read_text()
            fields = raw[raw.rindex(")") + 2:].split()
            return fields[19] == token and fields[0] not in ("Z", "X")
        except (OSError, ValueError, IndexError):
            return False
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


def terminate_worker(process, grace=0.3):
    """Cancel the whole request tree, even descendants in separate sessions."""
    if process is None:
        return
    marker = getattr(process, "request_id", None)
    descendants = _process_tree(process.pid, marker, process.poll() is None)
    if process.pid in descendants:
        _signal_owned(process.pid, descendants[process.pid], signal.SIGSTOP)
    for _ in range(3):
        current = _process_tree(process.pid, marker, process.poll() is None)
        added = {pid: token for pid, token in current.items() if pid not in descendants}
        descendants.update(current)
        for pid, token in descendants.items():
            _signal_owned(pid, token, signal.SIGSTOP)
        if not added:
            break
    for pid, token in reversed(list(descendants.items())):
        _signal_owned(pid, token, signal.SIGTERM)
        _signal_owned(pid, token, signal.SIGCONT)
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        process.poll()
        if not any(_is_alive(pid, token) for pid, token in descendants.items()):
            break
        time.sleep(0.01)
    for pid, token in reversed(list(descendants.items())):
        _signal_owned(pid, token, signal.SIGKILL)
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        process.poll()
        if not any(_is_alive(pid, token) for pid, token in descendants.items()):
            break
        time.sleep(0.01)
    try:
        process.wait(timeout=1)
    except subprocess.TimeoutExpired:
        pass
