"""
Bubblewrap (bwrap) physical process sandbox bridge for Makewand.
Enforces host protection, masks sensitive credential directories, and confines filesystem writes.
"""

import os
import shutil
from pathlib import Path
from typing import List, Tuple, Optional
from makewand.providers.base import run_subprocess

SENSITIVE_HOME_DIRS = [
    ".ssh",
    ".aws",
    ".gnupg",
    ".config/gcloud",
    ".azure",
    ".kube"
]

def is_bwrap_available() -> bool:
    bwrap = shutil.which("bwrap")
    if not bwrap:
        return False
    # Quick self-test
    ret, _, _, _ = run_subprocess([bwrap, "--ro-bind", "/", "/", "true"], timeout=3)
    return ret == 0

def wrap_bwrap(
    command_args: List[str],
    workspace: str,
    allow_network: bool = True
) -> List[str]:
    """
    Wraps command with bubblewrap isolating host filesystem and credentials.
    Only the specified workspace is writable.
    """
    bwrap = shutil.which("bwrap") or "/usr/bin/bwrap"
    ws = os.path.abspath(workspace)
    sandbox_home = os.path.join(ws, ".makewand_sandbox_home")
    os.makedirs(sandbox_home, exist_ok=True)

    path_env = os.environ.get("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
    user_home = str(Path.home())

    bwrap_cmd = [
        bwrap,
        "--die-with-parent",
        "--new-session",
        # Read-only root
        "--ro-bind", "/", "/",
        "--proc", "/proc",
        "--dev", "/dev",
        "--tmpfs", "/tmp",
        # Writable workspace only
        "--bind", ws, ws,
        "--chdir", ws,
        "--clearenv",
        "--setenv", "PATH", path_env,
        "--setenv", "HOME", sandbox_home,
        "--setenv", "TMPDIR", "/tmp",
        "--setenv", "MAKEWAND_SANDBOX", "1",
    ]

    if not allow_network:
        bwrap_cmd.append("--unshare-net")

    # Pass locale and terminal
    for var in ["LANG", "LC_ALL", "TERM"]:
        if var in os.environ:
            bwrap_cmd.extend(["--setenv", var, os.environ[var]])

    # Mask sensitive host directories in user's home
    for rel_dir in SENSITIVE_HOME_DIRS:
        sensitive_path = os.path.join(user_home, rel_dir)
        if os.path.exists(sensitive_path):
            bwrap_cmd.extend(["--tmpfs", sensitive_path])

    bwrap_cmd.extend(command_args)
    return bwrap_cmd

def run_in_sandbox(
    cmd: List[str],
    workspace: str,
    timeout: int = 120,
    allow_network: bool = True,
    stream: bool = False,
    print_prefix: str = ""
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command inside the bubblewrap sandbox.
    Falls back to normal subprocess if bwrap is not available.
    """
    if is_bwrap_available():
        wrapped = wrap_bwrap(cmd, workspace=workspace, allow_network=allow_network)
        return run_subprocess(
            wrapped,
            timeout=timeout,
            cwd=workspace,
            stream=stream,
            print_prefix=print_prefix
        )
    else:
        return run_subprocess(
            cmd,
            timeout=timeout,
            cwd=workspace,
            stream=stream,
            print_prefix=print_prefix
        )
