"""Bounded Windows pipes and process trees owned by a Windows Job Object.

Jobs contain resources and descendants; they do not isolate credentials, files
or networking. Host execution therefore retains the separate consent policy.
"""
import ctypes
import io
import math
import os
import queue
import re
import shutil
import subprocess
import sys
import threading
import time
from ctypes import wintypes


def resolve_windows_command(cmd):
    """Launch npm's Windows Node shims without interpreting model arguments.

    Passing a prompt through cmd.exe would expand %, &, pipes and redirections.
    Only the normal fixed npm JS entry is extracted; unknown batch launchers
    require a real executable rather than an implicit command shell.
    """
    if isinstance(cmd, str) or not isinstance(cmd, (list, tuple)) or not cmd:
        return cmd
    executable = shutil.which(os.fspath(cmd[0])) or os.fspath(cmd[0])
    if not executable.lower().endswith((".cmd", ".bat")):
        return [executable, *cmd[1:]]
    from pathlib import Path
    launcher = Path(executable)
    if launcher.stat().st_size > 65536:
        raise ValueError("Windows batch launcher exceeds the trusted shim limit")
    source = launcher.read_text(encoding="utf-8-sig")
    pattern = r'"%_prog%"\s+"%dp0%[\\/]?(node_modules[\\/][^"\r\n%]+\.js)"\s+%\*'
    matches = re.findall(pattern, source, re.I)
    if len(matches) != 1 or "node.exe" not in source.lower():
        raise ValueError("Unknown Windows batch launcher; provide its real executable or normal npm Node shim")
    relative = matches[0].replace("\\", "/")
    from makewand.native_windows import relative_parts
    entry = launcher.parent.joinpath(*relative_parts(relative))
    if not entry.is_file():
        raise OSError("Windows npm shim JS entry is unavailable: " + str(entry))
    node = launcher.parent / "node.exe"
    if not node.is_file():
        resolved = shutil.which("node.exe")
        if not resolved:
            raise OSError("Windows npm shim requires node.exe")
        node = Path(resolved)
    return [str(node), str(entry), *cmd[1:]]


class _BasicLimits(ctypes.Structure):
    _fields_ = [("process_time", ctypes.c_int64), ("job_time", ctypes.c_int64),
                ("flags", wintypes.DWORD), ("minimum_ws", ctypes.c_size_t),
                ("maximum_ws", ctypes.c_size_t), ("active_processes", wintypes.DWORD),
                ("affinity", ctypes.c_size_t), ("priority", wintypes.DWORD), ("scheduling", wintypes.DWORD)]


class _IoCounters(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint64) for name in ("read_ops", "write_ops", "other_ops", "read_bytes", "write_bytes", "other_bytes")]


class _ExtendedLimits(ctypes.Structure):
    _fields_ = [("basic", _BasicLimits), ("io", _IoCounters), ("process_memory", ctypes.c_size_t),
                ("job_memory", ctypes.c_size_t), ("peak_process_memory", ctypes.c_size_t), ("peak_job_memory", ctypes.c_size_t)]


class _ThreadEntry(ctypes.Structure):
    _fields_ = [("size", wintypes.DWORD), ("usage", wintypes.DWORD), ("thread_id", wintypes.DWORD),
                ("owner_pid", wintypes.DWORD), ("base_priority", wintypes.LONG),
                ("delta_priority", wintypes.LONG), ("flags", wintypes.DWORD)]


class WindowsJob:
    def __init__(self, *, active_process_limit=None, memory_limit_bytes=None):
        if os.name != "nt":
            raise OSError("Windows Job Objects require native Windows")
        self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        signatures = {
            "CreateJobObjectW": ([ctypes.c_void_p, wintypes.LPCWSTR], wintypes.HANDLE),
            "SetInformationJobObject": ([wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD], wintypes.BOOL),
            "AssignProcessToJobObject": ([wintypes.HANDLE, wintypes.HANDLE], wintypes.BOOL),
            "TerminateJobObject": ([wintypes.HANDLE, wintypes.UINT], wintypes.BOOL),
            "CloseHandle": ([wintypes.HANDLE], wintypes.BOOL),
            "CreateToolhelp32Snapshot": ([wintypes.DWORD, wintypes.DWORD], wintypes.HANDLE),
            "Thread32First": ([wintypes.HANDLE, ctypes.POINTER(_ThreadEntry)], wintypes.BOOL),
            "Thread32Next": ([wintypes.HANDLE, ctypes.POINTER(_ThreadEntry)], wintypes.BOOL),
            "OpenThread": ([wintypes.DWORD, wintypes.BOOL, wintypes.DWORD], wintypes.HANDLE),
            "ResumeThread": ([wintypes.HANDLE], wintypes.DWORD),
        }
        for name, (arguments, result) in signatures.items():
            getattr(self.kernel, name).argtypes = arguments
            getattr(self.kernel, name).restype = result
        self.handle = self.kernel.CreateJobObjectW(None, None)
        if not self.handle:
            raise ctypes.WinError(ctypes.get_last_error())
        limits = _ExtendedLimits()
        try:
            process_limit = active_process_limit if active_process_limit is not None else int(os.environ.get("MAKEWAND_WINDOWS_MAX_PROCESSES", "128"))
            memory_limit = memory_limit_bytes if memory_limit_bytes is not None else int(os.environ.get("MAKEWAND_WINDOWS_JOB_MEMORY_MB", "2048")) * 1024 * 1024
        except (TypeError, ValueError):
            self.close()
            raise ValueError("Windows Job resource limits must be integers") from None
        maximum_memory = min(16 * 1024 * 1024 * 1024, ctypes.c_size_t(-1).value)
        if (type(process_limit) is not int or type(memory_limit) is not int
                or not 1 <= process_limit <= 1024 or not 8 * 1024 * 1024 <= memory_limit <= maximum_memory):
            self.close()
            raise ValueError("Windows Job resource limits are outside their allowed bounds")
        limits.basic.flags = 0x2000 | 0x8 | 0x200  # KILL_ON_JOB_CLOSE | ACTIVE_PROCESS | JOB_MEMORY; no breakaway.
        limits.basic.active_processes = process_limit
        limits.job_memory = memory_limit
        if not self.kernel.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            error = ctypes.WinError(ctypes.get_last_error())
            self.close()
            raise error

    def start(self, cmd, **kwargs):
        # Python closes the primary thread handle inside Popen. Toolhelp opens
        # that still-suspended thread after assignment; no child code can run
        # before its job membership is established.
        proc = subprocess.Popen(resolve_windows_command(cmd), creationflags=0x4, **kwargs)  # CREATE_SUSPENDED
        try:
            if not self.kernel.AssignProcessToJobObject(self.handle, int(proc._handle)):
                raise ctypes.WinError(ctypes.get_last_error())
            snapshot = self.kernel.CreateToolhelp32Snapshot(0x4, 0)
            if snapshot == ctypes.c_void_p(-1).value:
                raise ctypes.WinError(ctypes.get_last_error())
            resumed = False
            try:
                entry = _ThreadEntry()
                entry.size = ctypes.sizeof(entry)
                found = self.kernel.Thread32First(snapshot, ctypes.byref(entry))
                while found:
                    if entry.owner_pid == proc.pid:
                        thread = self.kernel.OpenThread(0x2, False, entry.thread_id)
                        if not thread:
                            raise ctypes.WinError(ctypes.get_last_error())
                        try:
                            if self.kernel.ResumeThread(thread) == 0xFFFFFFFF:
                                raise ctypes.WinError(ctypes.get_last_error())
                            resumed = True
                        finally:
                            self.kernel.CloseHandle(thread)
                    found = self.kernel.Thread32Next(snapshot, ctypes.byref(entry))
            finally:
                self.kernel.CloseHandle(snapshot)
            if not resumed:
                raise OSError("Suspended Windows process has no resumable primary thread")
            proc._makewand_job = self
            return proc
        except BaseException:
            proc.kill()
            proc.wait(timeout=1)
            raise

    def terminate(self):
        if self.handle:
            if not self.kernel.TerminateJobObject(self.handle, 1):
                raise ctypes.WinError(ctypes.get_last_error())

    def close(self):
        if self.handle:
            self.kernel.CloseHandle(self.handle)
            self.handle = None


def run_windows_subprocess(cmd, timeout=180, cwd=None, input_text=None, stream=False,
                           print_prefix="", pass_fds=(), *, output_limit, stream_queue_limit):
    from makewand.providers.base import ProcessExecutionError
    from makewand.providers.base import _STREAM_WRITER
    import codecs
    proc = None
    job = None
    display_job = None
    display_proc = None
    threads = []
    stop = threading.Event()
    events = queue.Queue(maxsize=16)
    display_events = queue.Queue(maxsize=max(1, stream_queue_limit // 65536))
    captured = {"stdout": bytearray(), "stderr": bytearray()}
    total = 0
    target = sys.stdout
    decoder = codecs.getincrementaldecoder("utf-8")(errors="replace") if stream else None

    def enqueue(kind, value):
        while not stop.is_set():
            try:
                events.put((kind, value), timeout=.05)
                return
            except queue.Full:
                pass

    def read_pipe(kind, pipe):
        try:
            while not stop.is_set():
                data = pipe.read(65536)
                if not data:
                    break
                enqueue(kind, data)
        except (OSError, ValueError) as error:
            if not stop.is_set():
                enqueue("error", error)
        finally:
            enqueue("eof", kind)

    def write_input(pipe, payload, *, display=False):
        try:
            if display:
                while not stop.is_set():
                    try:
                        payload = display_events.get(timeout=.05)
                    except queue.Empty:
                        continue
                    if payload is None:
                        break
                    view = memoryview(payload)
                    while view and not stop.is_set():
                        view = view[pipe.write(view):]
            else:
                view = memoryview(payload)
                while view and not stop.is_set():
                    view = view[pipe.write(view[:65536]):]
        except (OSError, ValueError) as error:
            if display and not stop.is_set():
                enqueue("error", error)
        finally:
            pipe.close()

    def spawn_thread(function, *args, **kwargs):
        thread = threading.Thread(target=function, args=args, kwargs=kwargs, daemon=True)
        threads.append(thread)
        thread.start()

    def display(data, final=False):
        nonlocal display_job, display_proc
        if decoder is None:
            return
        text = decoder.decode(data, final=final)
        if not text:
            return
        if print_prefix:
            text = text.replace("\n", "\n" + print_prefix + " ")
        if type(target) is io.StringIO:
            target.write(text)
            return
        if display_proc is None:
            try:
                fd = target.fileno()
            except (AttributeError, OSError, ValueError) as error:
                raise RuntimeError("Stream display requires a file descriptor or built-in StringIO") from error
            display_job = WindowsJob()
            writer_code = "import msvcrt,os\nmsvcrt.setmode(0,os.O_BINARY)\nmsvcrt.setmode(1,os.O_BINARY)\n" + _STREAM_WRITER
            display_proc = display_job.start([sys.executable, "-I", "-u", "-c", writer_code],
                                            stdin=subprocess.PIPE, stdout=fd, stderr=subprocess.DEVNULL,
                                            bufsize=0, cwd=os.environ.get("SystemRoot", "C:\\Windows"),
                                            env={"SystemRoot": os.environ.get("SystemRoot", "C:\\Windows")})
            spawn_thread(write_input, display_proc.stdin, None, display=True)
        encoded = text.encode(getattr(target, "encoding", None) or "utf-8", errors="replace")
        # Prefix expansion is included in the display resource bound.
        for offset in range(0, len(encoded), 65536):
            try:
                display_events.put_nowait(encoded[offset:offset + 65536])
            except queue.Full as error:
                raise RuntimeError("Stream display exceeded its bounded Windows queue") from error

    def result(code, error=None):
        out = captured["stdout"].decode("utf-8", errors="replace")
        err = captured["stderr"].decode("utf-8", errors="replace")
        if not stream:
            out, err = (value.replace("\r\n", "\n").replace("\r", "\n") for value in (out, err))
        return code, out, err, error

    try:
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("Command timeout must be finite and positive")
        if pass_fds:
            raise ValueError("POSIX pass_fds cannot be used on native Windows")
        deadline = time.monotonic() + timeout
        job = WindowsJob()
        proc = job.start(cmd, shell=isinstance(cmd, str), cwd=cwd,
                         stdin=subprocess.PIPE if input_text is not None else None,
                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT if stream else subprocess.PIPE,
                         text=False, bufsize=0)
        pending = {"stdout"}
        spawn_thread(read_pipe, "stdout", proc.stdout)
        if proc.stderr is not None:
            pending.add("stderr")
            spawn_thread(read_pipe, "stderr", proc.stderr)
        if proc.stdin is not None:
            payload = input_text.encode("utf-8") if isinstance(input_text, str) else input_text or b""
            spawn_thread(write_input, proc.stdin, payload)
        group_cleaned = False
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                finished = proc.poll() is not None
                message = "Stream display did not finish before the command deadline" if finished and display_proc else f"Command timed out after {timeout} seconds"
                return result(-1, ProcessExecutionError(message, "UNKNOWN" if finished and display_proc else "TIMEOUT"))
            if proc.poll() is not None and not group_cleaned:
                job.terminate()
                group_cleaned = True
            if not pending and proc.poll() is not None:
                display(b"", final=True)
                if display_proc:
                    try:
                        display_events.put(None, timeout=min(.05, remaining))
                    except queue.Full:
                        continue
                    try:
                        display_proc.wait(timeout=remaining)
                    except subprocess.TimeoutExpired:
                        return result(-1, ProcessExecutionError("Stream display did not finish before the command deadline", "UNKNOWN"))
                    if display_proc.returncode:
                        raise RuntimeError("Windows stream writer failed")
                ret = proc.returncode
                return result(ret, ProcessExecutionError(f"Command returned exit code {ret}", "UNKNOWN") if ret else None)
            try:
                kind, value = events.get(timeout=min(.05, remaining))
            except queue.Empty:
                continue
            if kind == "error":
                raise value
            if kind == "eof":
                pending.discard(value)
                continue
            available = output_limit - total
            accepted = value[:available]
            captured[kind].extend(accepted)
            total += len(accepted)
            display(accepted)
            if len(value) > available:
                return result(-1, ProcessExecutionError(f"Command output exceeded shared {output_limit}-byte stdout/stderr limit", "UNKNOWN"))
    except KeyboardInterrupt:
        raise
    except Exception as error:
        return result(-1, ProcessExecutionError(str(error), "UNKNOWN" if proc is not None else "FAILED"))
    finally:
        stop.set()
        for active_job in (job, display_job):
            if active_job:
                active_job.close()
        for active_proc in (proc, display_proc):
            if active_proc:
                try:
                    active_proc.wait(timeout=.5)
                except subprocess.TimeoutExpired:
                    active_proc.kill()
                    active_proc.wait(timeout=.5)
        for thread in threads:
            thread.join(timeout=.5)
        for active_proc in (proc, display_proc):
            if active_proc:
                for pipe in (active_proc.stdout, active_proc.stderr, active_proc.stdin):
                    if pipe and not pipe.closed:
                        pipe.close()
