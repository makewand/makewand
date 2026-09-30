"""
Base provider execution runner with process-group isolation, non-blocking streaming, and rigorous timeout cleanup.
"""

import os
import sys
import time
import signal
import shutil
import codecs
import selectors
import subprocess
import threading
from typing import Tuple, Optional

MAX_OUTPUT_BYTES = 10 * 1024 * 1024  # 10 MB output guardrail


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
    Terminates the entire process tree associated with the given Popen instance.
    Uses process group SIGTERM -> wait grace period -> SIGKILL to guarantee all grandchildren are reaped.
    """
    if proc is None:
        return
    try:
        pgid = os.getpgid(proc.pid)
    except (ProcessLookupError, OSError):
        pgid = None

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
    Executes a command with process group isolation and true non-blocking streaming.
    Guarantees that timeout fires even if child outputs partial lines without newlines.
    Returns: (returncode, stdout, stderr, exception_or_timeout_msg)
    """
    proc = None
    try:
        popen_kwargs = {"pass_fds": pass_fds} if pass_fds else {}
        if not stream:
            proc = subprocess.Popen(
                cmd,
                shell=True if isinstance(cmd, str) else False,
                stdin=subprocess.PIPE if input_text is not None else None,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                cwd=cwd,
                start_new_session=True,
                **popen_kwargs
            )
            try:
                stdout, stderr = proc.communicate(input=input_text, timeout=timeout)
                error = ProcessExecutionError(f"Command returned exit code {proc.returncode}", "UNKNOWN") if proc.returncode else None
                return proc.returncode, stdout, stderr, error
            except subprocess.TimeoutExpired:
                kill_process_tree(proc)
                try:
                    stdout, stderr = proc.communicate(timeout=0.5)
                except Exception:
                    stdout, stderr = "", ""
                return -1, stdout or "", stderr or "", ProcessExecutionError(f"Command timed out after {timeout} seconds", "TIMEOUT")

        else:
            proc = subprocess.Popen(
                cmd,
                shell=True if isinstance(cmd, str) else False,
                stdin=subprocess.PIPE if input_text is not None else None,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=False,
                cwd=cwd,
                start_new_session=True,
                **popen_kwargs
            )
            if input_text is not None:
                def _feed_stdin(p, data):
                    try:
                        payload = data.encode("utf-8") if isinstance(data, str) else data
                        p.stdin.write(payload)
                        p.stdin.flush()
                    except Exception:
                        pass
                    finally:
                        try:
                            p.stdin.close()
                        except Exception:
                            pass

                stdin_thread = threading.Thread(target=_feed_stdin, args=(proc, input_text), daemon=True)
                stdin_thread.start()

            # Set stdout to non-blocking mode to prevent readline deadlocks on partial lines
            os.set_blocking(proc.stdout.fileno(), False)
            decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")

            collected = []
            total_bytes = 0
            deadline = time.monotonic() + timeout

            sel = selectors.DefaultSelector()
            sel.register(proc.stdout, selectors.EVENT_READ)

            try:
                while True:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        kill_process_tree(proc)
                        return -1, "".join(collected), "", ProcessExecutionError(f"Command timed out after {timeout} seconds", "TIMEOUT")

                    events = sel.select(timeout=min(0.2, max(0.05, remaining)))
                    for key, mask in events:
                        try:
                            chunk = proc.stdout.read(4096)
                        except (BlockingIOError, InterruptedError):
                            chunk = None

                        if chunk:
                            text_chunk = decoder.decode(chunk)
                            if text_chunk:
                                if total_bytes < MAX_OUTPUT_BYTES:
                                    collected.append(text_chunk)
                                    total_bytes += len(chunk)
                                if print_prefix:
                                    # Prefix lines
                                    prefixed = text_chunk.replace("\n", f"\n{print_prefix} ")
                                    sys.stdout.write(prefixed)
                                else:
                                    sys.stdout.write(text_chunk)
                                sys.stdout.flush()

                    if proc.poll() is not None:
                        # Drain any remaining bytes in pipe
                        try:
                            while True:
                                chunk = proc.stdout.read(4096)
                                if not chunk:
                                    break
                                text_chunk = decoder.decode(chunk)
                                if text_chunk:
                                    if total_bytes < MAX_OUTPUT_BYTES:
                                        collected.append(text_chunk)
                                        total_bytes += len(chunk)
                                    if print_prefix:
                                        prefixed = text_chunk.replace("\n", f"\n{print_prefix} ")
                                        sys.stdout.write(prefixed)
                                    else:
                                        sys.stdout.write(text_chunk)
                                    sys.stdout.flush()
                        except Exception:
                            pass
                        # Flush final decoded characters
                        final_text = decoder.decode(b"", final=True)
                        if final_text:
                            collected.append(final_text)
                            sys.stdout.write(final_text)
                            sys.stdout.flush()
                        break

                ret = proc.wait()
                error = ProcessExecutionError(f"Command returned exit code {ret}", "UNKNOWN") if ret else None
                return ret, "".join(collected), "", error
            finally:
                sel.close()

    except KeyboardInterrupt:
        if proc:
            kill_process_tree(proc)
        raise
    except Exception as e:
        if proc:
            kill_process_tree(proc)
        return -1, "", "", ProcessExecutionError(str(e), "UNKNOWN" if proc is not None else "FAILED")
    finally:
        if proc:
            for pipe in (getattr(proc, "stdout", None), getattr(proc, "stderr", None), getattr(proc, "stdin", None)):
                if pipe and not getattr(pipe, "closed", True):
                    try:
                        pipe.close()
                    except Exception:
                        pass
