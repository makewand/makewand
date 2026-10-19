"""Non-Linux lease lifecycle observations (no provider commands).

Windows process handles prove one PID's lifetime, not the death of its Job tree.
Darwin ps start times have one-second resolution; missing start times retain the
PID-reuse ambiguity. Inaccessible observations always keep capacity reserved.
"""
import os
import subprocess
import sys


def _windows_snapshot(pid):
    import ctypes
    from ctypes import wintypes
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
    kernel.OpenProcess.restype = wintypes.HANDLE
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    kernel.CloseHandle.restype = wintypes.BOOL
    kernel.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
    kernel.WaitForSingleObject.restype = wintypes.DWORD
    kernel.GetProcessTimes.argtypes = [wintypes.HANDLE] + [ctypes.POINTER(wintypes.FILETIME)] * 4
    kernel.GetProcessTimes.restype = wintypes.BOOL
    if not 0 < pid <= 0xffffffff:
        return None, None
    handle = kernel.OpenProcess(0x1000 | 0x100000, False, pid)
    if not handle:
        # ERROR_INVALID_PARAMETER means that this PID does not exist;
        # access denied and all other errors are inconclusive.
        return (False, None) if ctypes.get_last_error() == 87 else (None, None)
    try:
        status = kernel.WaitForSingleObject(handle, 0)
        if status == 0:
            return False, None
        if status != 258:  # WAIT_TIMEOUT: process is still running
            return None, None
        created, exited, kernel_time, user_time = (wintypes.FILETIME() for _ in range(4))
        token = None
        if kernel.GetProcessTimes(handle, *(ctypes.byref(t) for t in (created, exited, kernel_time, user_time))):
            token = str((created.dwHighDateTime << 32) | created.dwLowDateTime)
        return True, token
    finally:
        kernel.CloseHandle(handle)


def _darwin_rows(pid=None):
    args = ["/bin/ps", "-axo" if pid is None else "-o", "pid=,pgid=,stat=,lstart="]
    if pid is not None:
        args += ["-p", str(pid)]
    result = subprocess.run(args, capture_output=True, text=True, timeout=2,
                            env={**os.environ, "LC_ALL": "C", "TZ": "UTC"})
    if result.returncode != 0:
        raise OSError("process observation unavailable")
    rows = {}
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) != 8:
            raise ValueError("invalid process observation")
        rows[int(fields[0])] = (int(fields[1]), fields[2], " ".join(fields[3:]))
    return rows


def _pid_alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except OSError:
        return True  # permission denied is not evidence of death
    return True


def process_ref(pid):
    ref = {"pid": pid, "token": None, "boot": sys.platform, "group": None}
    if sys.platform == "win32":
        ref["boot"] = "windows"
        _, ref["token"] = _windows_snapshot(pid)
    elif sys.platform == "darwin":
        try:
            group, _, token = _darwin_rows(pid)[pid]
            ref.update(group=group, token=token)
        except (OSError, ValueError, KeyError, subprocess.SubprocessError):
            try:
                ref["group"] = os.getpgid(pid)
            except OSError:
                pass
    return ref


def alive(ref, group=False):
    if sys.platform == "win32":
        if ref.get("boot") not in (None, "windows"):
            return True
        live, token = _windows_snapshot(ref["pid"])
        # A dead leader cannot prove all Job descendants have stopped.
        # Only runner cleanup may release a registered Windows worker tree.
        if group:
            return True
        if live is False:
            return False
        return not (live and token and ref.get("token") and token != ref["token"])
    if sys.platform != "darwin" or ref.get("boot") not in (None, "darwin"):
        return True
    try:
        rows = _darwin_rows(None if group else ref["pid"])
        row = rows.get(ref["pid"])
        if row and row[1][0] not in ("Z", "X") and (not ref.get("token") or row[2] == ref["token"]):
            return True
        if group:
            if not ref.get("group"):
                return True
            return any(g == ref["group"] and state[0] not in ("Z", "X") for g, state, _ in rows.values())
        return False
    except (OSError, ValueError, subprocess.SubprocessError):
        # Without start time, a reused PID cannot be distinguished. Retain
        # the lease until PID/group death or explicit runner tree cleanup.
        if group:
            return True if not ref.get("group") else _pid_alive(-ref["group"])
        return _pid_alive(ref["pid"])
