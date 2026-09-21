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
    ".kube",
    ".claude",
    ".codex",
    ".gemini",
    ".anthropic",
    ".openai",
    ".config/gh",
    ".gitconfig",
    ".netrc"
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
    allow_network: bool = True,
    readonly: bool = False
) -> List[str]:
    """
    Wraps command with bubblewrap isolating host filesystem, IPC, PID, and credentials.
    Workspace is mounted read-only if readonly=True, otherwise writable.
    """
    bwrap = shutil.which("bwrap") or "/usr/bin/bwrap"
    ws = os.path.abspath(workspace)

    path_env = os.environ.get("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
    user_home = str(Path.home())

    bwrap_cmd = [
        bwrap,
        "--die-with-parent",
        "--new-session",
        "--unshare-pid",
        "--unshare-ipc",
        "--unshare-uts",
        # Read-only root
        "--ro-bind", "/", "/",
        "--proc", "/proc",
        "--dev", "/dev",
        "--tmpfs", "/tmp",
        # Ephemeral clean HOME in tmpfs to avoid workspace pollution
        "--tmpfs", user_home,
        # Workspace mount: ro-bind if readonly, otherwise bind
        "--ro-bind" if readonly else "--bind", ws, ws,
        "--chdir", ws,
        "--clearenv",
        "--setenv", "PATH", path_env,
        "--setenv", "HOME", user_home,
        "--setenv", "TMPDIR", "/tmp",
        "--setenv", "MAKEWAND_SANDBOX", "1",
    ]

    if not allow_network:
        bwrap_cmd.append("--unshare-net")

    # Pass safe locale and terminal
    for var in ["LANG", "LC_ALL", "TERM"]:
        if var in os.environ:
            bwrap_cmd.extend(["--setenv", var, os.environ[var]])

    # Explicitly mask sensitive host credential directories
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
    readonly: bool = False,
    stream: bool = False,
    print_prefix: str = ""
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command inside the bubblewrap sandbox.
    Enforces fail-closed security: refuses execution if bwrap is missing unless
    explicitly overridden by MAKEWAND_UNSAFE_HOST_EXEC=1.
    """
    exec_cmd = cmd
    if is_bwrap_available():
        exec_cmd = wrap_bwrap(cmd, workspace=workspace, allow_network=allow_network, readonly=readonly)
    elif os.environ.get("MAKEWAND_UNSAFE_HOST_EXEC") != "1":
        return (
            -1,
            "",
            "Bubblewrap (bwrap) sandbox is not available and MAKEWAND_UNSAFE_HOST_EXEC is not set. Execution blocked for security.",
            "SandboxUnavailable"
        )

    # Dynamic backpressure: when host load is elevated, deprioritize background sandbox task
    try:
        if os.getloadavg()[0] > 10.0:
            nice_bin = shutil.which("nice")
            if nice_bin:
                exec_cmd = [nice_bin, "-n", "10"] + exec_cmd
    except Exception:
        pass

    return run_subprocess(
        exec_cmd,
        timeout=timeout,
        cwd=workspace,
        stream=stream,
        print_prefix=print_prefix
    )
