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
from typing import Tuple, Optional

MAX_OUTPUT_BYTES = 10 * 1024 * 1024  # 10 MB output guardrail

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
    print_prefix: str = ""
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command with process group isolation and true non-blocking streaming.
    Guarantees that timeout fires even if child outputs partial lines without newlines.
    Returns: (returncode, stdout, stderr, exception_or_timeout_msg)
    """
    proc = None
    try:
        if not stream:
            proc = subprocess.Popen(
                cmd,
                shell=True if isinstance(cmd, str) else False,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                cwd=cwd,
                start_new_session=True
            )
            try:
                stdout, stderr = proc.communicate(input=input_text, timeout=timeout)
                return proc.returncode, stdout, stderr, None
            except subprocess.TimeoutExpired:
                kill_process_tree(proc)
                try:
                    stdout, stderr = proc.communicate(timeout=0.5)
                except Exception:
                    stdout, stderr = "", ""
                return -1, stdout or "", stderr or "", f"Command timed out after {timeout} seconds"

        else:
            proc = subprocess.Popen(
                cmd,
                shell=True if isinstance(cmd, str) else False,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=False,
                cwd=cwd,
                start_new_session=True
            )
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
                        return -1, "".join(collected), "", f"Command timed out after {timeout} seconds"

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
                return ret, "".join(collected), "", None
            finally:
                sel.close()

    except KeyboardInterrupt:
        if proc:
            kill_process_tree(proc)
        raise
    except Exception as e:
        if proc:
            kill_process_tree(proc)
        return -1, "", "", str(e)
