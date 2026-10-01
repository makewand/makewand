"""
Base provider execution runner with process-group isolation, non-blocking streaming, and rigorous timeout cleanup.
"""

import os
import io
import sys
import time
import signal
import shutil
import codecs
import math
import selectors
import subprocess
from typing import Tuple, Optional

MAX_OUTPUT_BYTES = 10 * 1024 * 1024  # 10 MB output guardrail
MAX_STREAM_QUEUE_BYTES = 256 * 1024
_STREAM_WRITER = (
    "import os\n"
    "while True:\n"
    " data = os.read(0, 65536)\n"
    " if not data: break\n"
    " while data:\n"
    "  written = os.write(1, data)\n"
    "  data = data[written:]\n"
)


class ProcessExecutionError(str):
    """String-compatible runner evidence; never derived from model output."""
    def __new__(cls, message, status):
        value = super().__new__(cls, message)
        value.execution_status = status
        return value


def is_process_timeout(error):
    """Timeout evidence belongs to the runner, never partial model output."""
    import re
    return (getattr(error, "execution_status", None) == "TIMEOUT"
            or isinstance(error, str) and re.fullmatch(r"Command timed out after [0-9.]+ seconds", error) is not None)


def model_process_failure(engine, code, output, stderr=None, exception=None, readonly=False):
    """A local process exit cannot establish a remote model's final outcome.

    Call this only after explicit configuration/quota/auth rejection handling.
    Spawn errors are known preflight failures; an admitted process's generic
    nonzero termination is unknown, including positive exit codes and stderr.
    """
    from makewand.execution_contract import ExecutionResult
    status = getattr(exception, "execution_status", None)
    if status is None and is_process_timeout(exception):
        status = "TIMEOUT"  # compatibility with runner fixtures/older callers
    status = status or "UNKNOWN"
    # Keep useful stderr on generic exits without treating its text as proof of
    # remote completion. Timeout/spawn evidence takes precedence over stderr.
    error = (exception or stderr) if status in ("TIMEOUT", "FAILED") else (stderr or exception)
    error = error or f"{engine} returned exit code {code}"
    return ExecutionResult(False, output, str(error), status=status, engine=engine,
        readonly=readonly, outcome_known=status == "FAILED",
        error_kind="deadline" if status == "TIMEOUT" else "process_start" if status == "FAILED" else "process_exit")

def check_cli_installed(bin_name: str) -> bool:
    return shutil.which(bin_name) is not None

def kill_process_tree(proc: subprocess.Popen, timeout_grace: float = 0.3):
    """
    Terminates this runner's new-session process group, even after its leader exits.
    Descendants that deliberately leave that process group require a stronger
    sandbox/cgroup boundary and are not covered by process-group cleanup.
    """
    if proc is None:
        return
    if os.name == "nt":
        job = getattr(proc, "_makewand_job", None)
        if job is not None:
            job.terminate()
        else:
            # Only pre-assignment spawn cleanup reaches this fallback. Native
            # execution is started suspended and must join its job first.
            proc.kill()
        try:
            proc.wait(timeout=.5)
        except subprocess.TimeoutExpired:
            pass
        return
    # Every process created below uses start_new_session=True: its initial PGID
    # is its PID. Looking up the leader after poll()/wait() would lose the group
    # when that leader has exited but children still hold its pipes or keep running.
    pgid = proc.pid

    # Step 1: SIGTERM to process group
    if pgid is not None:
        try:
            os.killpg(pgid, signal.SIGTERM)
        except (ProcessLookupError, OSError):
            pass
    else:
        try:
            proc.terminate()
        except (ProcessLookupError, OSError):
            pass

    # Wait grace period
    t0 = time.monotonic()
    while time.monotonic() - t0 < timeout_grace:
        if proc.poll() is not None:
            break
        time.sleep(0.05)

    # Step 2: SIGKILL to process group to ensure child and grandchildren terminate
    if pgid is not None:
        try:
            os.killpg(pgid, signal.SIGKILL)
        except (ProcessLookupError, OSError):
            pass
    else:
        try:
            proc.kill()
        except (ProcessLookupError, OSError):
            pass

    try:
        proc.wait(timeout=0.3)
    except Exception:
        pass

def run_subprocess(
    cmd,
    timeout: int = 180,
    cwd: Optional[str] = None,
    input_text: Optional[str] = None,
    stream: bool = False,
    print_prefix: str = "",
    pass_fds: tuple = ()
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command with one deadline and a shared raw stdout/stderr byte limit.
    Pipe reads and stdin writes are non-blocking, including partial output lines.
    FD-backed stream display uses a killable writer process and a fixed-size
    queue; blocking consumers cannot stop the parent deadline loop. Exact built-in
    StringIO is also supported; arbitrary Python write callbacks are refused.
    Cleanup has a bounded termination grace after the deadline. Bytes already
    written cannot be undone; uninterruptible kernel I/O is not a hard-real-time
    cancellation guarantee.
    Returns: (returncode, stdout, stderr, exception_or_timeout_msg)
    """
    if os.name == "nt":
        from makewand.windows_process import run_windows_subprocess
        return run_windows_subprocess(cmd, timeout, cwd, input_text, stream, print_prefix, pass_fds,
                                      output_limit=MAX_OUTPUT_BYTES, stream_queue_limit=MAX_STREAM_QUEUE_BYTES)
    proc = None
    selector = None
    cleaned_group = False
    captured = {"stdout": bytearray(), "stderr": bytearray()}
    total_bytes = 0
    decoder = codecs.getincrementaldecoder("utf-8")(errors="replace") if stream else None
    display_target = sys.stdout
    display_proc = None
    display_queue = bytearray()
    display_finalized = False
    cleaned_display = False

    def cleanup_group():
        nonlocal cleaned_group
        if proc is not None and not cleaned_group:
            cleaned_group = True
            kill_process_tree(proc)

    def cleanup_display():
        nonlocal cleaned_display
        if display_proc is not None and not cleaned_display:
            cleaned_display = True
            kill_process_tree(display_proc)

    def close_pipe(pipe):
        if pipe is None or pipe.closed:
            return
        if selector is not None:
            try:
                selector.unregister(pipe)
            except KeyError:
                pass
        if pipe is not None and not pipe.closed:
            pipe.close()

    def display(data, *, final=False):
        nonlocal display_proc, display_finalized
        if decoder is None:
            return
        if display_finalized:
            return
        text = decoder.decode(data, final=final)
        display_finalized = final
        if text:
            text = text.replace("\n", f"\n{print_prefix} ") if print_prefix else text
            if type(display_target) is io.StringIO:
                display_target.write(text)
                return
            if display_proc is None:
                try:
                    target_fd = display_target.fileno()
                except (AttributeError, OSError, ValueError) as error:
                    raise RuntimeError("Stream display requires a file descriptor or built-in StringIO") from error
                # Fixed code only, no task/account environment or workspace loading.
                # This process owns the potentially blocking output write; it can
                # be terminated without changing the caller's stdout flags.
                display_proc = subprocess.Popen(
                    [sys.executable, "-I", "-u", "-c", _STREAM_WRITER],
                    stdin=subprocess.PIPE, stdout=target_fd, stderr=subprocess.DEVNULL,
                    bufsize=0, start_new_session=True, env={"PATH": os.defpath}, cwd="/"
                )
                os.set_blocking(display_proc.stdin.fileno(), False)
            encoded = text.encode(getattr(display_target, "encoding", None) or "utf-8", errors="replace")
            if len(display_queue) + len(encoded) > MAX_STREAM_QUEUE_BYTES:
                raise RuntimeError(f"Stream display exceeded its {MAX_STREAM_QUEUE_BYTES}-byte queue limit")
            display_queue.extend(encoded)
            if display_proc.stdin not in selector.get_map():
                selector.register(display_proc.stdin, selectors.EVENT_WRITE, "display")

    def result(code, error=None, *, flush_display=True):
        if flush_display:
            display(b"", final=True)
        out = captured["stdout"].decode("utf-8", errors="replace")
        err = captured["stderr"].decode("utf-8", errors="replace")
        if not stream:  # Preserve the former text-mode universal-newline behavior.
            out = out.replace("\r\n", "\n").replace("\r", "\n")
            err = err.replace("\r\n", "\n").replace("\r", "\n")
        return code, out, err, error

    try:
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("Command timeout must be finite and positive")
        popen_kwargs = {"pass_fds": pass_fds} if pass_fds else {}
        deadline = time.monotonic() + timeout
        proc = subprocess.Popen(
            cmd,
            shell=isinstance(cmd, str),
            stdin=subprocess.PIPE if input_text is not None else None,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT if stream else subprocess.PIPE,
            text=False,
            bufsize=0,
            cwd=cwd,
            start_new_session=True,
            **popen_kwargs
        )
        selector = selectors.DefaultSelector()
        for name, pipe in (("stdout", proc.stdout), ("stderr", proc.stderr)):
            if pipe is not None:
                os.set_blocking(pipe.fileno(), False)
                selector.register(pipe, selectors.EVENT_READ, name)
        payload = memoryview(input_text.encode("utf-8") if isinstance(input_text, str) else input_text or b"")
        input_offset = 0
        if proc.stdin is not None:
            os.set_blocking(proc.stdin.fileno(), False)
            if payload:
                selector.register(proc.stdin, selectors.EVENT_WRITE, "stdin")
            else:
                close_pipe(proc.stdin)

        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                provider_finished = proc.poll() is not None
                cleanup_group()
                if provider_finished and display_proc is not None:
                    return result(-1, ProcessExecutionError("Stream display did not finish before the command deadline", "UNKNOWN"),
                                  flush_display=False)
                return result(-1, ProcessExecutionError(f"Command timed out after {timeout} seconds", "TIMEOUT"),
                              flush_display=False)

            if display_proc is not None and display_proc.poll() is not None:
                if display_proc.returncode or not display_proc.stdin.closed:
                    raise RuntimeError("Stream display process ended before output was complete")

            ret = proc.poll()
            if ret is not None:
                cleanup_group()
                close_pipe(proc.stdin)
                if not any(key.data in ("stdout", "stderr") for key in selector.get_map().values()):
                    display(b"", final=True)
                    if display_proc is not None and not display_queue:
                        close_pipe(display_proc.stdin)
                    if display_proc is None or display_proc.poll() is not None:
                        error = ProcessExecutionError(f"Command returned exit code {ret}", "UNKNOWN") if ret else None
                        return result(ret, error)

            for key, _ in selector.select(timeout=min(0.05, remaining)):
                if key.data == "display":
                    try:
                        written = os.write(key.fd, display_queue[:65536])
                    except (BlockingIOError, InterruptedError):
                        continue
                    del display_queue[:written]
                    if not display_queue:
                        selector.unregister(key.fileobj)
                    continue
                if key.data == "stdin":
                    try:
                        input_offset += os.write(key.fd, payload[input_offset:input_offset + 65536])
                    except (BrokenPipeError, ConnectionResetError):
                        close_pipe(key.fileobj)
                    except (BlockingIOError, InterruptedError):
                        continue
                    else:
                        if input_offset == len(payload):
                            close_pipe(key.fileobj)
                    continue

                try:
                    chunk = os.read(key.fd, min(65536, MAX_OUTPUT_BYTES - total_bytes + 1))
                except (BlockingIOError, InterruptedError):
                    continue
                if not chunk:
                    close_pipe(key.fileobj)
                    continue
                available = MAX_OUTPUT_BYTES - total_bytes
                accepted = chunk[:available]
                captured[key.data].extend(accepted)
                total_bytes += len(accepted)
                display(accepted)
                if len(chunk) > available:
                    cleanup_group()
                    return result(-1, ProcessExecutionError(
                        f"Command output exceeded shared {MAX_OUTPUT_BYTES}-byte stdout/stderr limit", "UNKNOWN"),
                                  flush_display=False)

    except KeyboardInterrupt:
        raise
    except Exception as e:
        cleanup_group()
        return result(-1, ProcessExecutionError(str(e), "UNKNOWN" if proc is not None else "FAILED"),
                      flush_display=False)
    finally:
        cleanup_group()
        cleanup_display()
        if selector is not None:
            selector.close()
        if proc is not None:
            for pipe in (getattr(proc, "stdout", None), getattr(proc, "stderr", None), getattr(proc, "stdin", None)):
                if pipe and not getattr(pipe, "closed", True):
                    try:
                        pipe.close()
                    except Exception:
                        pass
        if display_proc is not None and not display_proc.stdin.closed:
            display_proc.stdin.close()
