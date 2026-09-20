"""
Base provider execution runner with process-group isolation, non-blocking streaming, and rigorous timeout cleanup.
"""

import os
import sys
import time
import signal
import shutil
import selectors
import subprocess
from typing import Tuple, Optional

MAX_OUTPUT_BYTES = 10 * 1024 * 1024  # 10 MB output guardrail

def check_cli_installed(bin_name: str) -> bool:
    return shutil.which(bin_name) is not None

def kill_process_tree(proc: subprocess.Popen, timeout_grace: float = 0.5):
    """
    Terminates the entire process tree associated with the given Popen instance.
    Uses process group SIGTERM -> wait -> SIGKILL.
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
            return
        time.sleep(0.05)

    # Step 2: SIGKILL if still alive
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
        proc.wait(timeout=0.5)
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
    Executes a command with process group isolation and non-blocking streaming.
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
                text=True,
                cwd=cwd,
                bufsize=1,
                start_new_session=True
            )
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

                    events = sel.select(timeout=min(0.2, remaining))
                    for key, mask in events:
                        line = proc.stdout.readline()
                        if line:
                            if total_bytes < MAX_OUTPUT_BYTES:
                                collected.append(line)
                                total_bytes += len(line.encode('utf-8', errors='replace'))
                            if print_prefix:
                                sys.stdout.write(f"{print_prefix} {line}")
                            else:
                                sys.stdout.write(line)
                            sys.stdout.flush()

                    if proc.poll() is not None:
                        # Drain any remaining lines in buffer
                        try:
                            for line in proc.stdout:
                                if line:
                                    if total_bytes < MAX_OUTPUT_BYTES:
                                        collected.append(line)
                                        total_bytes += len(line.encode('utf-8', errors='replace'))
                                    if print_prefix:
                                        sys.stdout.write(f"{print_prefix} {line}")
                                    else:
                                        sys.stdout.write(line)
                                    sys.stdout.flush()
                        except Exception:
                            pass
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
