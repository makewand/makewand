"""
Base provider execution runner with line-by-line live streaming.
"""

import sys
import time
import shutil
import subprocess
from typing import Tuple, Optional

def check_cli_installed(bin_name: str) -> bool:
    return shutil.which(bin_name) is not None

def run_subprocess(
    cmd,
    timeout: int = 180,
    cwd: Optional[str] = None,
    input_text: Optional[str] = None,
    stream: bool = False,
    print_prefix: str = ""
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command with optional live streaming.
    Returns: (returncode, stdout, stderr, exception_or_timeout_msg)
    """
    try:
        if not stream:
            res = subprocess.run(
                cmd,
                shell=True if isinstance(cmd, str) else False,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                timeout=timeout,
                cwd=cwd,
                input=input_text
            )
            return res.returncode, res.stdout, res.stderr, None
        else:
            proc = subprocess.Popen(
                cmd,
                shell=True if isinstance(cmd, str) else False,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                cwd=cwd,
                bufsize=1
            )
            collected = []
            start_time = time.time()
            while True:
                line = proc.stdout.readline()
                if not line and proc.poll() is not None:
                    break
                if line:
                    collected.append(line)
                    if print_prefix:
                        sys.stdout.write(f"{print_prefix} {line}")
                    else:
                        sys.stdout.write(line)
                    sys.stdout.flush()
                if time.time() - start_time > timeout:
                    proc.kill()
                    return -1, "".join(collected), "", f"Command timed out after {timeout} seconds"
            ret = proc.wait()
            return ret, "".join(collected), "", None
    except subprocess.TimeoutExpired:
        return -1, "", "", f"Command timed out after {timeout} seconds"
    except Exception as e:
        return -1, "", "", str(e)
