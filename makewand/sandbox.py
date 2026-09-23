"""
Bubblewrap (bwrap) physical process sandbox bridge for Makewand.
Enforces host protection, masks sensitive credential directories, and confines filesystem writes.
"""

import os
import sys
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
    ".git-credentials",
    ".cargo/credentials.toml",
    ".cargo/credentials",
    ".cargo/config.toml",
    ".npmrc",
    ".pypirc",
    ".netrc",
    ".docker"
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
    readonly: bool = False,
    repo_root: Optional[str] = None,
    is_provider: bool = False,
    worktree_root: Optional[str] = None,
    extra_env: Optional[dict] = None
) -> List[str]:
    """
    Wraps command with bubblewrap isolating host filesystem, IPC, PID, and credentials.
    Workspace (or entire worktree root if workspace is a subdirectory) is mounted.
    Workspace is mounted read-only if readonly=True, otherwise writable.
    If is_provider=True, authorized provider credentials (.claude, .codex) are mounted read-only.
    If is_provider=False (default for user code / test executions), HOME is isolated with tmpfs.
    """
    bwrap = shutil.which("bwrap") or "/usr/bin/bwrap"
    ws = os.path.abspath(workspace)
    user_home = str(Path.home())

    # Determine worktree mount root to ensure full repo visibility when running from a subdirectory
    # NEVER execute git rev-parse inside untrusted workspaces, which can expand mounts via core.worktree
    mount_root = None
    if worktree_root:
        wt_abs = os.path.abspath(worktree_root)
        try:
            if Path(ws).resolve().is_relative_to(Path(wt_abs).resolve()):
                mount_root = wt_abs
        except Exception:
            pass

    if not mount_root:
        try:
            curr = Path(ws).resolve()
            boundaries = {
                Path.home().resolve(),
                Path("/tmp").resolve(),
                Path("/var/tmp").resolve(),
                Path("/").resolve(),
            }
            if (curr / ".git").exists():
                mount_root = str(curr)
            else:
                p = curr.parent
                while p != curr and p not in boundaries:
                    if (p / ".git").exists():
                        mount_root = str(p)
                        break
                    p = p.parent
        except Exception:
            pass

    if not mount_root:
        mount_root = ws

    bwrap_cmd = [
        bwrap,
        "--die-with-parent",
        "--new-session",
        "--unshare-pid",
        "--unshare-ipc",
        "--unshare-uts",
        "--clearenv",
        # Read-only host root filesystem
        "--ro-bind", "/", "/",
        "--proc", "/proc",
        "--dev", "/dev",
        "--tmpfs", "/tmp",
    ]

    # HOME isolation policy:
    if is_provider:
        # Provider tools (claude, codex, agy, muse) run as user and need user home for CLI runtime configs
        bwrap_cmd.extend(["--ro-bind", user_home, user_home])
        # Allow provider authentication tokens to be read without permitting modifications to host configs
        for auth_dir in [".claude", ".codex"]:
            auth_path = os.path.join(user_home, auth_dir)
            if os.path.exists(auth_path):
                bwrap_cmd.extend(["--ro-bind", auth_path, auth_path])
                sessions_dir = os.path.join(auth_path, "sessions")
                if os.path.exists(sessions_dir) and not readonly:
                    bwrap_cmd.extend(["--tmpfs", sessions_dir])
    else:
        # General user code / test execution: completely isolate HOME with tmpfs to prevent credential leakage
        bwrap_cmd.extend(["--tmpfs", user_home])
        # Mount ONLY executable tool directories (never parent directories containing credential files)
        SAFE_HOME_BIN_DIRS = [
            ".cargo/bin",
            ".local/bin",
            ".nvm/versions",
            ".pyenv/shims",
            ".pyenv/versions",
            ".rustup/toolchains",
            ".nix-profile/bin"
        ]
        for bin_sub in SAFE_HOME_BIN_DIRS:
            tp = os.path.join(user_home, bin_sub)
            if os.path.exists(tp):
                bwrap_cmd.extend(["--ro-bind", tp, tp])

        # Mount active Python interpreter / venv if located in user home
        mounted_home_paths = set()
        try:
            py_exe = os.path.abspath(sys.executable)
            if py_exe.startswith(user_home):
                py_dir = os.path.dirname(py_exe)
                py_env_root = os.path.dirname(py_dir)
                if os.path.exists(py_env_root) and py_env_root != user_home:
                    bwrap_cmd.extend(["--ro-bind", py_env_root, py_env_root])
                    mounted_home_paths.add(py_env_root)
                elif os.path.exists(py_dir):
                    bwrap_cmd.extend(["--ro-bind", py_dir, py_dir])
                    mounted_home_paths.add(py_dir)
        except Exception:
            pass

        # If command executable itself is located under user_home, mount it read-only (unless parent was already mounted)
        try:
            if command_args and os.path.isabs(command_args[0]):
                cmd_bin = os.path.abspath(command_args[0])
                if cmd_bin.startswith(user_home) and os.path.exists(cmd_bin):
                    already_mounted = any(cmd_bin.startswith(mp + "/") or cmd_bin == mp for mp in mounted_home_paths)
                    if not already_mounted and not any(cmd_bin.startswith(os.path.join(user_home, s)) for s in SENSITIVE_HOME_DIRS):
                        bwrap_cmd.extend(["--ro-bind", cmd_bin, cmd_bin])
        except Exception:
            pass

    # Mount workspace / worktree
    if mount_root == ws:
        bwrap_cmd.extend(["--ro-bind" if readonly else "--bind", ws, ws])
    else:
        # Mount the entire worktree root so subdirectories have full visibility of root files, siblings, and .git
        bwrap_cmd.extend(["--ro-bind" if readonly else "--bind", mount_root, mount_root])

    # If repo_root is provided and distinct from mount_root, ensure host repo_root is strictly read-only
    if repo_root:
        repo_abs = os.path.abspath(repo_root)
        if repo_abs != mount_root and repo_abs != ws:
            bwrap_cmd.extend(["--ro-bind", repo_abs, repo_abs])

    bwrap_cmd.extend([
        "--chdir", ws,
        "--setenv", "HOME", user_home,
        "--setenv", "TMPDIR", "/tmp",
        "--setenv", "MAKEWAND_SANDBOX", "1",
    ])

    if readonly:
        bwrap_cmd.extend(["--setenv", "MAKEWAND_READONLY", "1"])

    # Strict environment whitelist: only pass safe system variables to general sandbox,
    # pass provider API variables ONLY when is_provider is explicitly True
    SAFE_PASSTHROUGH_ENVS = [
        "PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ",
        "NODE_PATH", "PYTHONPATH"
    ]
    if is_provider:
        SAFE_PASSTHROUGH_ENVS.extend([
            "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "META_API_KEY", "GEMINI_API_KEY",
            "CODEX_API_KEY", "CLAUDE_API_KEY"
        ])

    for var in SAFE_PASSTHROUGH_ENVS:
        if var in os.environ:
            bwrap_cmd.extend(["--setenv", var, os.environ[var]])

    if extra_env:
        for k, v in extra_env.items():
            bwrap_cmd.extend(["--setenv", str(k), str(v)])

    if not allow_network:
        bwrap_cmd.append("--unshare-net")

    # Universally mask sensitive host credential directories and files
    for rel_dir in SENSITIVE_HOME_DIRS:
        if is_provider and rel_dir in [".claude", ".codex"]:
            continue
        sensitive_path = os.path.join(user_home, rel_dir)
        if os.path.exists(sensitive_path):
            real_path = os.path.realpath(sensitive_path)
            if os.path.isdir(real_path):
                bwrap_cmd.extend(["--tmpfs", real_path])
            elif os.path.isfile(real_path):
                bwrap_cmd.extend(["--ro-bind", "/dev/null", real_path])

    # Universally mask user-configured protected production trees
    try:
        from makewand.git_helper import get_protected_paths
        for prod_path in get_protected_paths():
            if prod_path.exists():
                real_prod = str(prod_path.resolve())
                if not (mount_root.startswith(real_prod) or ws.startswith(real_prod)):
                    bwrap_cmd.extend(["--tmpfs", real_prod])
    except Exception:
        pass

    bwrap_cmd.extend(command_args)
    return bwrap_cmd

def run_in_sandbox(
    cmd: List[str],
    workspace: str,
    timeout: int = 120,
    allow_network: bool = True,
    readonly: bool = False,
    repo_root: Optional[str] = None,
    is_provider: bool = False,
    worktree_root: Optional[str] = None,
    stream: bool = False,
    print_prefix: str = "",
    extra_env: Optional[dict] = None
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command inside the bubblewrap sandbox.
    Enforces fail-closed security: refuses execution if bwrap is missing unless
    explicitly overridden by MAKEWAND_UNSAFE_HOST_EXEC=1.
    """
    exec_cmd = cmd
    if is_bwrap_available():
        exec_cmd = wrap_bwrap(
            cmd,
            workspace=workspace,
            allow_network=allow_network,
            readonly=readonly,
            repo_root=repo_root,
            is_provider=is_provider,
            worktree_root=worktree_root,
            extra_env=extra_env,
        )
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
