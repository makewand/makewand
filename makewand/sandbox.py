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
    ".docker",
    ".config/muse",
    ".local/share/muse",
    ".grok"
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
    extra_env: Optional[dict] = None,
    provider_name: Optional[str] = None
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

    # Identify provider strictly from explicit provider_name or first command argument binary name
    p_name = (provider_name or "").lower().strip()
    if not p_name and command_args:
        first_bin = os.path.basename(str(command_args[0])).lower()
        for candidate in ["claude", "codex", "agy", "muse", "grok"]:
            if first_bin == candidate or first_bin.startswith(candidate + "-") or first_bin.startswith(candidate + "."):
                p_name = candidate
                break

    is_muse = is_provider and (p_name == "muse")
    is_claude = is_provider and (p_name == "claude")
    is_codex = is_provider and (p_name == "codex")
    is_agy = is_provider and (p_name == "agy")
    is_grok = is_provider and (p_name == "grok")

    bwrap_cmd = [
        bwrap,
        "--die-with-parent",
        "--new-session",
    ]
    if not is_muse:
        bwrap_cmd.append("--unshare-pid")
    bwrap_cmd.extend([
        "--unshare-ipc",
        "--unshare-uts",
        "--clearenv",
        # Read-only host root filesystem
        "--ro-bind", "/", "/",
        "--proc", "/proc",
        "--dev", "/dev",
        "--tmpfs", "/tmp",
    ])

    # S05 defense: Mask system Unix domain sockets and host IPC runtimes
    for sock_runtime_dir in ["/var/tmp", "/run", "/var/run"]:
        if os.path.exists(sock_runtime_dir) and not os.path.islink(sock_runtime_dir):
            bwrap_cmd.extend(["--tmpfs", sock_runtime_dir])

    # HOME isolation policy: Always isolate HOME with tmpfs to prevent credential, history and token leakage
    bwrap_cmd.extend(["--tmpfs", user_home])

    # Mount safe executable toolchain directories (never parent directories containing credential files)
    SAFE_HOME_BIN_DIRS = [
        ".cargo/bin",
        ".local/bin",
        ".nvm",
        ".pyenv/shims",
        ".pyenv/versions",
        ".rustup/toolchains",
        ".nix-profile/bin"
    ]
    if is_claude:
        SAFE_HOME_BIN_DIRS.append(".local/share/claude")
    elif is_muse:
        SAFE_HOME_BIN_DIRS.append(".local/libexec")
    elif is_grok:
        SAFE_HOME_BIN_DIRS.append(".grok/bin")

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
                if not already_mounted:
                    bwrap_cmd.extend(["--ro-bind", cmd_bin, cmd_bin])
    except Exception:
        pass

    # If is_provider=True, mount ONLY the active provider's specific auth/config paths (never others)
    provider_auth_dirs = []
    if is_provider:
        provider_auth_paths = []
        if is_claude:
            provider_auth_paths.extend([(".claude", False), (".claude.json", True)])
            provider_auth_dirs.append(".claude")
        elif is_codex:
            provider_auth_paths.append((".codex", False))
            provider_auth_dirs.append(".codex")
        elif is_muse:
            provider_auth_paths.extend([(".config/muse", False), (".local/share/muse", False)])
            provider_auth_dirs.extend([".config/muse", ".local/share/muse"])
        elif is_agy:
            provider_auth_paths.append((".gemini", False))
            provider_auth_dirs.append(".gemini")
        elif is_grok:
            provider_auth_paths.append((".grok", False))
            provider_auth_dirs.append(".grok")

        for auth_rel, ro_file in provider_auth_paths:
            auth_path = os.path.join(user_home, auth_rel)
            if os.path.exists(auth_path):
                real_p = os.path.realpath(auth_path)
                bind_flag = "--ro-bind" if (ro_file or readonly) else "--bind"
                if real_p != auth_path and os.path.exists(real_p):
                    bwrap_cmd.extend([bind_flag, real_p, real_p])
                bwrap_cmd.extend([bind_flag, auth_path, auth_path])

        # Protect sensitive config files and hook directories within auth dirs from tampering even in writable sessions
        if not readonly:
            for sc_rel in [
                ".claude/settings.json",
                ".claude/settings.local.json",
                ".claude/hooks",
                ".codex/config.toml",
                ".config/muse/settings.json"
            ]:
                sc_p = os.path.join(user_home, sc_rel)
                if os.path.exists(sc_p):
                    bwrap_cmd.extend(["--ro-bind", sc_p, sc_p])

        if is_muse:
            uid = os.getuid()
            run_user = f"/run/user/{uid}"
            if os.path.exists(run_user):
                bwrap_cmd.extend(["--ro-bind", run_user, run_user])

    # Mount workspace / worktree
    if mount_root == ws:
        bwrap_cmd.extend(["--ro-bind" if readonly else "--bind", ws, ws])
    else:
        # Mount the entire worktree root so subdirectories have full visibility of root files, siblings, and .git
        bwrap_cmd.extend(["--ro-bind" if readonly else "--bind", mount_root, mount_root])

    # In writable sessions, protect all root .git directories AND submodule .git gitfiles from tampering
    if not readonly:
        protected_git_paths = set()
        for m_dir in {ws, mount_root}:
            for root, dirs, files in os.walk(m_dir):
                if ".git" in dirs:
                    protected_git_paths.add(os.path.join(root, ".git"))
                    dirs.remove(".git")
                if ".git" in files:
                    protected_git_paths.add(os.path.join(root, ".git"))

        for gp_str in sorted(protected_git_paths):
            bwrap_cmd.extend(["--ro-bind", gp_str, gp_str])

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
    # pass provider API variables ONLY tailored to the active provider when is_provider is True
    SAFE_PASSTHROUGH_ENVS = [
        "PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ",
        "NODE_PATH", "PYTHONPATH"
    ]
    if allow_network:
        SAFE_PASSTHROUGH_ENVS.extend([
            "http_proxy", "https_proxy", "all_proxy", "no_proxy",
            "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"
        ])
    if is_provider:
        if is_claude:
            SAFE_PASSTHROUGH_ENVS.extend(["ANTHROPIC_API_KEY", "CLAUDE_API_KEY"])
        elif is_codex:
            SAFE_PASSTHROUGH_ENVS.extend(["OPENAI_API_KEY", "CODEX_API_KEY"])
        elif is_grok:
            SAFE_PASSTHROUGH_ENVS.extend(["XAI_API_KEY", "GROK_API_KEY", "GROK_AUTH_TOKEN", "GROK_WEB_FETCH_PROXY"])
        elif is_muse:
            SAFE_PASSTHROUGH_ENVS.extend(["META_API_KEY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"])
        elif is_agy:
            SAFE_PASSTHROUGH_ENVS.extend(["GEMINI_API_KEY"])

    for var in SAFE_PASSTHROUGH_ENVS:
        if var in os.environ:
            bwrap_cmd.extend(["--setenv", var, os.environ[var]])

    if extra_env:
        for k, v in extra_env.items():
            bwrap_cmd.extend(["--setenv", str(k), str(v)])

    if not allow_network:
        bwrap_cmd.append("--unshare-net")

    # Universally mask user-configured protected production trees that lie OUTSIDE user_home
    # (Paths inside user_home are already completely absent thanks to user_home being an isolated tmpfs)
    try:
        from makewand.git_helper import get_protected_paths
        for prod_path in get_protected_paths():
            if prod_path.exists():
                real_prod = str(prod_path.resolve())
                if not real_prod.startswith(user_home) and not (mount_root.startswith(real_prod) or ws.startswith(real_prod)):
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
        load_1m = os.getloadavg()[0]
        if load_1m > 10.0:
            nice_bin = shutil.which("nice")
            if nice_bin:
                nice_val = "15" if load_1m > 18.0 else "10"
                exec_cmd = [nice_bin, "-n", nice_val] + exec_cmd
            ionice_bin = shutil.which("ionice")
            if ionice_bin and load_1m > 12.0:
                exec_cmd = [ionice_bin, "-c2", "-n7"] + exec_cmd
    except Exception:
        pass

    return run_subprocess(
        exec_cmd,
        timeout=timeout,
        cwd=workspace,
        stream=stream,
        print_prefix=print_prefix
    )
